package collector

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"kula/internal/config"
)

func TestLogSafe(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain text, with \"quotes\" and a \\ backslash", "plain text, with \"quotes\" and a \\ backslash"},
		{"naïve – ünïcödé €", "naïve – ünïcödé €"},
		{"one\nforged line", `one\nforged line`},
		{"carriage\rreturn\ttab", `carriage\rreturn\ttab`},
		{"\x1b]0;owned\a\x1b[2J", `\x1b]0;owned\a\x1b[2J`},
		{"right-to-left \u202eoverride", `right-to-left \u202eoverride`},
		{"invalid \xff\xfe bytes", `invalid \xff\xfe bytes`},
		{"a real \uFFFD stays", "a real \uFFFD stays"},
		{"", ""},
	} {
		if got := logSafe(tc.in); got != tc.want {
			t.Errorf("logSafe(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRedactURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"http://localhost/status", "http://localhost/status"},
		{"http://monitor:s3cret@localhost:8080/server-status?auto", "http://xxxxx@localhost:8080/server-status?auto"},
		{"https://token@status.example.com/", "https://xxxxx@status.example.com/"},
		{"http://user:s3cr/et@localhost/status", "http://xxxxx@localhost/status"}, // does not parse
		{"http://user:p%zz@localhost/", "http://xxxxx@localhost/"},                // does not parse
	} {
		if got := redactURL(tc.in); got != tc.want {
			t.Errorf("redactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestStatusPageLogsHideCredentials: a status URL may carry the credentials
// of a protected page. Neither the startup line nor a failure shows them,
// including the parser's complaint about a URL that does not parse, which
// quotes the part it stumbled on.
func TestStatusPageLogsHideCredentials(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()

	for _, tc := range []struct{ name, url string }{
		{"unreachable", "http://kulauser:s3cret@127.0.0.1:" + closedPort + "/status"},
		{"does not parse", "http://kulauser:s3cr/et@127.0.0.1:" + closedPort + "/status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLog(t)
			apps := config.ApplicationsConfig{
				Nginx:   config.NginxConfig{Enabled: true, StatusURL: tc.url},
				Apache2: config.Apache2Config{Enabled: true, StatusURL: tc.url},
			}
			c := New(config.GlobalConfig{}, config.CollectionConfig{Interval: time.Second}, apps, t.TempDir())
			c.StartApplications()
			deadline := time.Now().Add(10 * time.Second)
			for strings.Count(logs.String(), "fetch error") < 2 && strings.Count(logs.String(), "bad status URL") < 2 {
				if time.Now().After(deadline) {
					t.Fatalf("no failure logged:\n%s", logs.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			c.Stop()
			c.nginxPoller.wait(5 * time.Second)
			c.apachePoller.wait(5 * time.Second)

			got := logs.String()
			for _, secret := range []string{"kulauser", "s3cr"} {
				if strings.Contains(got, secret) {
					t.Fatalf("the log shows %q:\n%s", secret, got)
				}
			}
			for _, want := range []string{"[nginx] monitoring enabled at http://xxxxx@127.0.0.1", "[apache2] monitoring enabled at http://xxxxx@127.0.0.1"} {
				if !strings.Contains(got, want) {
					t.Fatalf("the log lacks %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestStatusBodyDebugLogIsEscaped: the debug log shows the status page as one
// escaped line, so the page cannot forge log lines or send terminal escapes.
func TestStatusBodyDebugLogIsEscaped(t *testing.T) {
	logs := captureLog(t)
	page := "Active connections: 1 \x1b]0;owned\a\n2026/01/01 00:00:00 [kula] forged line\nserver accepts handled requests\n 1 1 1\nReading: 0 Writing: 1 Waiting: 0\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, page)
	}))
	defer server.Close()
	c := &Collector{
		nginxClient: &http.Client{},
		collCfg:     config.CollectionConfig{DebugLog: true},
		appCfg:      config.ApplicationsConfig{Nginx: config.NginxConfig{Enabled: true, StatusURL: server.URL}},
	}
	c.collectNginx(1)
	got := logs.String()
	if !strings.Contains(got, `[nginx] status body: "Active connections: 1 \x1b]0;owned\a\n2026/01/01 00:00:00 [kula] forged line\n`) {
		t.Fatalf("the status body was not logged escaped:\n%s", got)
	}
	if strings.ContainsAny(got, "\x1b\a") || strings.Contains(got, "\n2026/01/01 00:00:00 [kula] forged line") {
		t.Fatalf("the status body reached the log unescaped:\n%s", got)
	}
}

// errConnector fails every connection attempt with err.
type errConnector struct{ err error }

func (c errConnector) Connect(context.Context) (driver.Conn, error) { return nil, c.err }
func (errConnector) Driver() driver.Driver                          { return fakeDBDriver{} }

// TestDBSessionLogEscapesServerText: a connection error can carry whatever
// the server, or whatever answered on its port, chose to send. It is logged
// on one line, escaped.
func TestDBSessionLogEscapesServerText(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	s := &dbSession{
		name:      "[mysql]",
		label:     "db",
		connector: errConnector{errors.New("Error 1045: denied\n2026/01/01 00:00:00 [mysql] polling \"db\" again\x1b[2J")},
		timeouts:  newDBTimeouts(time.Second),
		logf: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, fmt.Sprintf(format, args...))
		},
		dialect: dbDialect{describe: func(dbSessionRef) string { return "" }},
	}
	if _, err := s.acquire(context.Background()); err == nil {
		t.Fatal("acquire succeeded")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 || strings.ContainsAny(lines[0], "\n\x1b") ||
		!strings.Contains(lines[0], `denied\n2026/01/01 00:00:00 [mysql] polling "db" again\x1b[2J`) {
		t.Fatalf("logged %q; want one line with the server's text escaped", lines)
	}
}

// ioStateConnector serves SHOW REPLICA STATUS with one row whose IO state is
// state.
type ioStateConnector struct{ state string }

func (c ioStateConnector) Connect(context.Context) (driver.Conn, error) {
	return &ioStateConn{state: c.state}, nil
}
func (ioStateConnector) Driver() driver.Driver { return fakeDBDriver{} }

type ioStateConn struct {
	panicConn
	state string
}

func (c *ioStateConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if query != "SHOW REPLICA STATUS" {
		return nil, errors.New("unsupported")
	}
	return &ioStateRows{state: c.state}, nil
}

type ioStateRows struct {
	state string
	done  bool
}

func (r *ioStateRows) Columns() []string { return []string{"Replica_IO_State", "Replica_IO_Running"} }
func (r *ioStateRows) Close() error      { return nil }
func (r *ioStateRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0], dest[1] = r.state, "Yes"
	return nil
}

// TestMysqlIOStateIsCutOnACharacter: the replica IO state is the server's
// text, capped at 200 bytes for storage. The cap falls on a character
// boundary and the stored state is valid UTF-8, whatever the server sent.
func TestMysqlIOStateIsCutOnACharacter(t *testing.T) {
	for _, tc := range []struct{ name, state, want string }{
		{"short", "Waiting for source to send event", "Waiting for source to send event"},
		{"three-byte characters", strings.Repeat("€", 100), strings.Repeat("€", 66)},
		{"invalid UTF-8", "Waiting \xff\xfe", "Waiting \uFFFD"},
		{"exactly 200 bytes", strings.Repeat("ab", 100), strings.Repeat("ab", 100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := sql.OpenDB(ioStateConnector{state: tc.state})
			defer func() { _ = db.Close() }()
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			stats := &MysqlStats{}
			(&mysqlCollector{}).replicaStatus(context.Background(), conn, stats)
			if stats.IOState != tc.want || !utf8.ValidString(stats.IOState) || len(stats.IOState) > 200 || !stats.ReplicaIORunning {
				t.Fatalf("IOState = %q (%d bytes, valid %v), want %q", stats.IOState, len(stats.IOState), utf8.ValidString(stats.IOState), tc.want)
			}
		})
	}
}
