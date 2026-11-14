package collector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"kula/internal/config"

	"github.com/go-sql-driver/mysql"
)

func TestNginxCollector(t *testing.T) {
	// Mock Nginx stub_status output
	stubOutput := `Active connections: 291 
server accepts handled requests
 16630948 16630948 31070465 
Reading: 6 Writing: 179 Waiting: 106 
`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(stubOutput))
	}))
	defer server.Close()

	c := &Collector{
		nginxClient: &http.Client{},
		appCfg: config.ApplicationsConfig{
			Nginx: config.NginxConfig{
				Enabled:   true,
				StatusURL: server.URL,
			},
		},
	}

	stats := c.collectNginx(1.0)
	if stats == nil {
		t.Fatal("Expected stats, got nil")
	}

	if stats.ActiveConnections != 291 {
		t.Errorf("Expected ActiveConnections 291, got %d", stats.ActiveConnections)
	}
	if stats.Reading != 6 || stats.Writing != 179 || stats.Waiting != 106 {
		t.Errorf("Unexpected R/W/W: %d/%d/%d", stats.Reading, stats.Writing, stats.Waiting)
	}

	// Test malformed output
	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not nginx output"))
	}))
	defer badServer.Close()

	c.appCfg.Nginx.StatusURL = badServer.URL
	stats = c.collectNginx(1.0)
	if stats != nil {
		t.Error("Expected nil stats for malformed output")
	}
}

func TestApache2Collector(t *testing.T) {
	stubOutput := `Total Accesses: 1234
Total kBytes: 5678
CPULoad: .12345
Uptime: 12345
ReqPerSec: .1
BytesPerSec: 470.7
BytesPerReq: 4707.21
BusyWorkers: 3
IdleWorkers: 7
Scoreboard: _W___R___K...............................................
`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(stubOutput))
	}))
	defer server.Close()

	c := &Collector{
		apacheClient: &http.Client{},
		appCfg: config.ApplicationsConfig{
			Apache2: config.Apache2Config{
				Enabled:   true,
				StatusURL: server.URL,
			},
		},
	}

	stats := c.collectApache2(1.0)
	if stats == nil {
		t.Fatal("Expected stats, got nil")
	}

	if stats.BusyWorkers != 3 {
		t.Errorf("Expected BusyWorkers 3, got %d", stats.BusyWorkers)
	}
	if stats.IdleWorkers != 7 {
		t.Errorf("Expected IdleWorkers 7, got %d", stats.IdleWorkers)
	}
	if stats.TotalAccesses != 1234 {
		t.Errorf("Expected TotalAccesses 1234, got %d", stats.TotalAccesses)
	}
	if stats.TotalKBytes != 5678 {
		t.Errorf("Expected TotalKBytes 5678, got %d", stats.TotalKBytes)
	}
	if stats.ReqPerSec != 0.1 {
		t.Errorf("Expected ReqPerSec 0.1, got %f", stats.ReqPerSec)
	}
	if stats.BytesPerSec != 470.7 {
		t.Errorf("Expected BytesPerSec 470.7, got %f", stats.BytesPerSec)
	}
	if stats.Uptime != 12345 {
		t.Errorf("Expected Uptime 12345, got %d", stats.Uptime)
	}
	// Scoreboard: _ (waiting), W (sending), R (reading), K (keepalive), . (open slot, ignored)
	if stats.Waiting < 1 || stats.Reading < 1 || stats.Sending < 1 || stats.Keepalive < 1 {
		t.Errorf("Expected non-zero scoreboard states: W=%d R=%d S=%d K=%d",
			stats.Waiting, stats.Reading, stats.Sending, stats.Keepalive)
	}
	// Open slots (.) should be tracked
	if stats.OpenSlots < 1 {
		t.Errorf("Expected non-zero open slots, got %d", stats.OpenSlots)
	}

	// Test malformed output returns nil
	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not apache output"))
	}))
	defer badServer.Close()

	c.appCfg.Apache2.StatusURL = badServer.URL
	stats = c.collectApache2(1.0)
	if stats != nil {
		t.Error("Expected nil stats for malformed output")
	}

	// Test empty scoreboard still produces valid stats
	noSB := `BusyWorkers: 1
IdleWorkers: 5
`
	noSBServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(noSB))
	}))
	defer noSBServer.Close()

	c.appCfg.Apache2.StatusURL = noSBServer.URL
	c.prevApache = apache2Raw{} // reset
	stats = c.collectApache2(1.0)
	if stats == nil {
		t.Fatal("Expected stats without scoreboard, got nil")
	}
	if stats.BusyWorkers != 1 || stats.IdleWorkers != 5 {
		t.Errorf("Expected 1 busy, 5 idle; got %d, %d", stats.BusyWorkers, stats.IdleWorkers)
	}

	// Test multi-line scoreboard (e.g. large MPM event config)
	multiSB := `BusyWorkers: 2
IdleWorkers: 10
Scoreboard: _W__R__K__...............................................
_________W_______R___________________K__________________.............
`
	multiSBServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(multiSB))
	}))
	defer multiSBServer.Close()

	c.appCfg.Apache2.StatusURL = multiSBServer.URL
	c.prevApache = apache2Raw{} // reset
	stats = c.collectApache2(1.0)
	if stats == nil {
		t.Fatal("Expected stats with multi-line scoreboard, got nil")
	}
	// Should count workers across BOTH scoreboard lines
	if stats.Waiting < 2 || stats.Reading < 2 || stats.Sending < 2 || stats.Keepalive < 2 {
		t.Errorf("Multi-line scoreboard undercounted: W=%d R=%d S=%d K=%d (all should be >= 2)",
			stats.Waiting, stats.Reading, stats.Sending, stats.Keepalive)
	}
	// Open slots (.) should be present from the dots in scoreboard
	if stats.OpenSlots < 10 {
		t.Errorf("Expected >= 10 open slots in multi-line test, got %d", stats.OpenSlots)
	}

	// Test all scoreboard states with dedicated characters
	allSBO := `Total Accesses: 100
BusyWorkers: 5
IdleWorkers: 5
Scoreboard: _SRWKDCLGI._SRWKDCLGI.
`
	allSBServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(allSBO))
	}))
	defer allSBServer.Close()

	c.appCfg.Apache2.StatusURL = allSBServer.URL
	c.prevApache = apache2Raw{} // reset
	stats = c.collectApache2(1.0)
	if stats == nil {
		t.Fatal("Expected stats with full scoreboard, got nil")
	}
	if stats.Waiting != 2 || stats.Starting != 2 || stats.Reading != 2 ||
		stats.Sending != 2 || stats.Keepalive != 2 || stats.DNS != 2 ||
		stats.Closing != 2 || stats.Logging != 2 || stats.Graceful != 2 ||
		stats.IdleCleanup != 2 || stats.OpenSlots != 2 {
		t.Errorf("Full scoreboard miscount: _=%d S=%d R=%d W=%d K=%d D=%d C=%d L=%d G=%d I=%d .=%d",
			stats.Waiting, stats.Starting, stats.Reading, stats.Sending, stats.Keepalive,
			stats.DNS, stats.Closing, stats.Logging, stats.Graceful, stats.IdleCleanup, stats.OpenSlots)
	}

	// Test counter reset (service restart): previous counters are high,
	// current counters reset to 0 — should NOT produce insane rates.
	c.appCfg.Apache2.StatusURL = server.URL
	c.prevApache = apache2Raw{totalAccesses: 50000000, totalKBytes: 100000000, hasAccesses: true, hasKBytes: true}
	resetOutput := `Total Accesses: 100
Total kBytes: 200
`
	resetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(resetOutput))
	}))
	defer resetServer.Close()
	c.appCfg.Apache2.StatusURL = resetServer.URL
	stats = c.collectApache2(1.0)
	if stats == nil {
		t.Fatal("Expected stats after counter reset, got nil")
	}
	if stats.AccessesPS != 0 || stats.KBytesPS != 0 {
		t.Errorf("Expected 0 PS after counter reset, got AccessesPS=%g KBytesPS=%g",
			stats.AccessesPS, stats.KBytesPS)
	}
}

func TestNginxCollectorCounterReset(t *testing.T) {
	stubOutput := `Active connections: 1 
server accepts handled requests
 100 100 200 
Reading: 0 Writing: 1 Waiting: 0 
`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(stubOutput))
	}))
	defer server.Close()

	c := &Collector{
		nginxClient: &http.Client{},
		appCfg: config.ApplicationsConfig{
			Nginx: config.NginxConfig{
				Enabled:   true,
				StatusURL: server.URL,
			},
		},
		prevNginx: nginxRaw{accepts: 50000000, handled: 50000000, requests: 100000000, seen: true},
	}

	stats := c.collectNginx(1.0)
	if stats == nil {
		t.Fatal("Expected stats after nginx counter reset, got nil")
	}
	if stats.AcceptsPS != 0 || stats.HandledPS != 0 || stats.RequestsPS != 0 {
		t.Errorf("Expected 0 PS after nginx counter reset, got A=%g H=%g R=%g",
			stats.AcceptsPS, stats.HandledPS, stats.RequestsPS)
	}
}

// TestNginxRatesAcrossPartialResetsAndFailures covers the deltas the reset
// guard used to miss: counters that went backwards while accepts did not, and
// a failed poll followed by a good one.
func TestNginxRatesAcrossPartialResetsAndFailures(t *testing.T) {
	body := func(accepts, handled, requests int) string {
		return "Active connections: 1\nserver accepts handled requests\n " +
			strconv.Itoa(accepts) + " " + strconv.Itoa(handled) + " " + strconv.Itoa(requests) +
			"\nReading: 0 Writing: 1 Waiting: 0\n"
	}
	var current string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if current == "" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(current))
	}))
	defer server.Close()
	c := &Collector{
		nginxClient: &http.Client{},
		appCfg:      config.ApplicationsConfig{Nginx: config.NginxConfig{Enabled: true, StatusURL: server.URL}},
	}
	poll := func(b string) *NginxStats {
		t.Helper()
		current = b
		return c.collectNginx(1)
	}

	if s := poll(body(1000, 1000, 3000)); s == nil || s.RequestsPS != 0 {
		t.Fatalf("first poll: %+v, want no rates without a baseline", s)
	}
	if s := poll(body(1010, 1010, 3050)); s == nil || s.AcceptsPS != 10 || s.RequestsPS != 50 {
		t.Fatalf("steady poll: %+v, want 10 accepts/s and 50 requests/s", s)
	}
	// nginx restarted and has already accepted more connections than before,
	// but its request counter started over.
	if s := poll(body(1500, 1500, 20)); s == nil || s.AcceptsPS != 0 || s.HandledPS != 0 || s.RequestsPS != 0 {
		t.Fatalf("partial reset: %+v, want no rates instead of an underflow", s)
	}
	if s := poll(body(1505, 1505, 40)); s == nil || s.RequestsPS != 20 {
		t.Fatalf("after reset: %+v, want 20 requests/s from the new baseline", s)
	}
	// A failed poll drops the baseline: the next good poll must not divide two
	// intervals of growth by one.
	if s := poll(""); s != nil {
		t.Fatalf("failed poll returned %+v", s)
	}
	if s := poll(body(1525, 1525, 140)); s == nil || s.RequestsPS != 0 {
		t.Fatalf("after a failed poll: %+v, want no rate until a new baseline", s)
	}
	if s := poll(body(1530, 1530, 150)); s == nil || s.RequestsPS != 10 {
		t.Fatalf("baseline restored: %+v, want 10 requests/s", s)
	}
}

// TestApache2RatesAcrossFailuresAndMissingCounters mirrors the nginx case for
// mod_status: a failed poll drops the baseline, and a page without a counter
// (ExtendedStatus off) is no baseline for that counter's rate.
func TestApache2RatesAcrossFailuresAndMissingCounters(t *testing.T) {
	body := func(accesses, kbytes int) string {
		return "Total Accesses: " + strconv.Itoa(accesses) + "\nTotal kBytes: " + strconv.Itoa(kbytes) +
			"\nBusyWorkers: 1\nIdleWorkers: 4\n"
	}
	var current string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if current == "" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(current))
	}))
	defer server.Close()
	c := &Collector{
		apacheClient: &http.Client{},
		appCfg:       config.ApplicationsConfig{Apache2: config.Apache2Config{Enabled: true, StatusURL: server.URL}},
	}
	poll := func(b string) *Apache2Stats {
		t.Helper()
		current = b
		return c.collectApache2(1)
	}

	if s := poll(body(1000, 5000)); s == nil || s.AccessesPS != 0 || s.KBytesPS != 0 {
		t.Fatalf("first poll: %+v, want no rates without a baseline", s)
	}
	if s := poll(body(1010, 5040)); s == nil || s.AccessesPS != 10 || s.KBytesPS != 40 {
		t.Fatalf("steady poll: %+v, want 10 accesses/s and 40 kB/s", s)
	}
	if s := poll(""); s != nil {
		t.Fatalf("failed poll returned %+v", s)
	}
	if s := poll(body(1030, 5120)); s == nil || s.AccessesPS != 0 || s.KBytesPS != 0 {
		t.Fatalf("after a failed poll: %+v, want no rates until a new baseline", s)
	}
	if s := poll(body(1035, 5130)); s == nil || s.AccessesPS != 5 || s.KBytesPS != 10 {
		t.Fatalf("baseline restored: %+v, want 5 accesses/s and 10 kB/s", s)
	}
	// ExtendedStatus switched off: the counters vanish from the page.
	if s := poll("BusyWorkers: 1\nIdleWorkers: 4\n"); s == nil || s.AccessesPS != 0 || s.KBytesPS != 0 {
		t.Fatalf("page without counters: %+v, want no rates", s)
	}
	if s := poll(body(1040, 5140)); s == nil || s.AccessesPS != 0 || s.KBytesPS != 0 {
		t.Fatalf("counters back: %+v, want no rates against the missing baseline", s)
	}
}

func TestPostgresCollectorMath(t *testing.T) {
	pc := &postgresCollector{
		prev: pgRaw{
			xactCommit: 100,
			blksRead:   50,
			blksHit:    50,
			valid:      true,
		},
	}

	cur := pgRaw{
		xactCommit: 150,
		blksRead:   60,
		blksHit:    140,
		valid:      true,
	}

	stats := &PostgresStats{}
	pc.calculateStats(stats, cur, 10.0) // 10 seconds elapsed

	if stats.TxCommitPS != 5.0 { // (150-100)/10
		t.Errorf("Expected 5.0 TPS, got %.2f", stats.TxCommitPS)
	}
	if stats.BlksHitPct != 70.0 { // 140 / (60+140) * 100
		t.Errorf("Expected 70%% hit ratio, got %.2f", stats.BlksHitPct)
	}
}

// TestPostgresRatesNeedValidCountersAtBothEnds covers a background-writer
// query that fails for one poll: its zeroed counters used to become the
// baseline, so the next poll reported lifetime totals as one interval's rate.
func TestPostgresRatesNeedValidCountersAtBothEnds(t *testing.T) {
	pc := &postgresCollector{}
	poll := func(commits, checkpoint int64, bgValid bool) *PostgresStats {
		t.Helper()
		stats := &PostgresStats{}
		pc.calculateStats(stats, pgRaw{xactCommit: commits, bufCheckpoint: checkpoint, valid: true, bgValid: bgValid}, 1)
		return stats
	}

	if s := poll(1000, 5_000_000, true); s.TxCommitPS != 0 || s.BufCheckpointPS != 0 {
		t.Fatalf("first poll: %+v, want no rates without a baseline", s)
	}
	if s := poll(1010, 5_000_040, true); s.TxCommitPS != 10 || s.BufCheckpointPS != 40 {
		t.Fatalf("steady poll: %+v", s)
	}
	if s := poll(1020, 0, false); s.TxCommitPS != 10 || s.BufCheckpointPS != 0 {
		t.Fatalf("bgwriter failure: %+v, want commits/s kept and no checkpoint rate", s)
	}
	if s := poll(1030, 5_000_100, true); s.TxCommitPS != 10 || s.BufCheckpointPS != 0 {
		t.Fatalf("bgwriter back: %+v, want no checkpoint rate until a new baseline (not 5e6/s)", s)
	}
	if s := poll(1040, 5_000_130, true); s.BufCheckpointPS != 30 {
		t.Fatalf("bgwriter baseline restored: %+v, want 30/s", s)
	}

	// A fresh database with no commits yet still establishes a baseline.
	pc = &postgresCollector{}
	poll(0, 0, true)
	if s := poll(5, 0, true); s.TxCommitPS != 5 {
		t.Fatalf("zero-commit baseline: %+v, want 5 commits/s", s)
	}
}

func TestPostgresDSN(t *testing.T) {
	// TCP
	pc1 := newPostgresCollector("localhost", 5432, "user", "pass", "db", "disable", false, time.Second)
	for _, want := range []string{"host='localhost'", "port=5432", "user='user'", "password='pass'",
		"application_name='kula'", "statement_timeout=5000", "lock_timeout=2000",
		"idle_in_transaction_session_timeout=10000", "tcp_keepalives_idle=30"} {
		if !strings.Contains(pc1.dsn, want) {
			t.Errorf("TCP DSN lacks %s: %s", want, pc1.dsn)
		}
	}

	// Unix socket
	pc2 := newPostgresCollector("/var/run/postgresql", 0, "user", "", "db", "disable", false, time.Second)
	if !strings.Contains(pc2.dsn, "host='/var/run/postgresql'") || strings.Contains(pc2.dsn, "port=") {
		t.Errorf("Unexpected Unix DSN: %s", pc2.dsn)
	}

	// A configured value cannot add parameters of its own.
	pc3 := newPostgresCollector("localhost", 5432, "user", "x' sslmode='disable", "db name", "verify-full", false, time.Second)
	if pc3.session.connector == nil {
		t.Fatalf("quoted DSN did not parse: %s", pc3.dsn)
	}
	if !strings.Contains(pc3.dsn, `password='x\' sslmode=\'disable'`) || !strings.Contains(pc3.dsn, "dbname='db name'") {
		t.Errorf("values not quoted: %s", pc3.dsn)
	}
}

func TestMysqlDSN(t *testing.T) {
	// TCP connection; the password holds DSN delimiters.
	mc1 := newMysqlCollector("localhost", 3306, "user", "p@ss/word:1", "mydb", "disable", "", false, time.Second)
	parsed, err := mysql.ParseDSN(mc1.config.FormatDSN())
	if err != nil {
		t.Fatalf("formatted DSN does not parse: %v", err)
	}
	if parsed.Net != "tcp" || parsed.Addr != "localhost:3306" || parsed.User != "user" ||
		parsed.Passwd != "p@ss/word:1" || parsed.DBName != "mydb" {
		t.Errorf("config round trip: %+v", parsed)
	}
	if parsed.Params["wait_timeout"] != "60" || parsed.Params["lock_wait_timeout"] != "2" ||
		parsed.ReadTimeout != 10*time.Second || parsed.ConnectionAttributes != "program_name:kula" {
		t.Errorf("session bounds missing: params=%v read=%s attrs=%q", parsed.Params, parsed.ReadTimeout, parsed.ConnectionAttributes)
	}

	// IPv6 hosts get brackets.
	if got := newMysqlCollector("::1", 3306, "user", "", "", "disable", "", false, time.Second).config.Addr; got != "[::1]:3306" {
		t.Errorf("IPv6 address = %q", got)
	}

	// Unix socket connection (port == 0)
	mc2 := newMysqlCollector("/var/run/mysqld/mysqld.sock", 0, "user", "", "mydb", "disable", "", false, time.Second)
	if mc2.config.Net != "unix" || mc2.config.Addr != "/var/run/mysqld/mysqld.sock" {
		t.Errorf("Unexpected Unix config: %s %s", mc2.config.Net, mc2.config.Addr)
	}
}

// TestMysqlAddressFromConfig: the address dialled and the label logged for
// each way of writing the host in the config file. "[::1]" worked in 0.21.0,
// which joined host and port by hand, and must not be bracketed twice.
func TestMysqlAddressFromConfig(t *testing.T) {
	for host, want := range map[string]string{
		`"[::1]"`:         "[::1]:3306",
		`"::1"`:           "[::1]:3306",
		"db.example":      "db.example:3306",
		"192.0.2.1":       "192.0.2.1:3306",
		`"[2001:db8::5]"`: "[2001:db8::5]:3306",
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		yaml := "storage:\n  directory: " + dir + "\napplications:\n  mysql:\n    host: " + host + "\n    port: 3306\n"
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.LoadRequired(path)
		if err != nil {
			t.Fatal(err)
		}
		my := cfg.Applications.Mysql
		mc := newMysqlCollector(my.Host, my.Port, my.User, my.Password, my.DBName, my.SSLMode, my.SSLRootCert, false, time.Second)
		if mc.config.Net != "tcp" || mc.config.Addr != want || mc.label != want {
			t.Errorf("host %s: dials %s %q, logged as %q; want tcp %q for both", host, mc.config.Net, mc.config.Addr, mc.label, want)
		}
		if _, _, err := net.SplitHostPort(mc.config.Addr); err != nil {
			t.Errorf("host %s: address %q does not split: %v", host, mc.config.Addr, err)
		}
	}
}

func TestMysqlCollectorMath(t *testing.T) {
	mc := &mysqlCollector{
		prev: mysqlRaw{
			questions:        100,
			comSelect:        50,
			comInsert:        10,
			comUpdate:        5,
			comDelete:        2,
			slowQueries:      1,
			innodbBPReads:    20,
			innodbBPRequests: 200,
			tableLocksWaited: 3,
			rowLockWaits:     4,
			valid:            true,
		},
	}

	cur := mysqlRaw{
		valid:            true,
		questions:        200,  // +100 over 10s → 10 q/s
		comSelect:        100,  // +50  → 5 select/s
		comInsert:        20,   // +10  → 1 insert/s
		comUpdate:        10,   // +5   → 0.5 update/s
		comDelete:        4,    // +2   → 0.2 delete/s
		slowQueries:      3,    // +2   → 0.2 slow/s
		innodbBPReads:    30,   // +10  → 1 bp_reads/s
		innodbBPRequests: 1030, // 1000 hits + 30 reads → 97.09% hit ratio
		tableLocksWaited: 8,    // +5   → 0.5/s
		rowLockWaits:     9,    // +5   → 0.5/s
	}

	stats := &MysqlStats{}
	mc.calculateStats(stats, cur, 10.0) // 10 seconds elapsed

	if stats.QueriesPS != 10.0 {
		t.Errorf("Expected QueriesPS 10.0, got %.2f", stats.QueriesPS)
	}
	if stats.ComSelectPS != 5.0 {
		t.Errorf("Expected ComSelectPS 5.0, got %.2f", stats.ComSelectPS)
	}
	if stats.ComInsertPS != 1.0 {
		t.Errorf("Expected ComInsertPS 1.0, got %.2f", stats.ComInsertPS)
	}
	if stats.SlowQueriesPS != 0.2 {
		t.Errorf("Expected SlowQueriesPS 0.2, got %.2f", stats.SlowQueriesPS)
	}
	if stats.InnodbBPReadsPS != 1.0 {
		t.Errorf("Expected InnodbBPReadsPS 1.0, got %.2f", stats.InnodbBPReadsPS)
	}
	// Buffer pool hit pct: (1030-30)/1030 * 100 = 97.087...%
	if stats.InnodbBufferPoolHitPct < 97.0 || stats.InnodbBufferPoolHitPct > 98.0 {
		t.Errorf("Expected InnodbBufferPoolHitPct ~97%%, got %.2f", stats.InnodbBufferPoolHitPct)
	}
	if stats.TableLocksWaitedPS != 0.5 {
		t.Errorf("Expected TableLocksWaitedPS 0.5, got %.2f", stats.TableLocksWaitedPS)
	}
	if stats.RowLockWaitsPS != 0.5 {
		t.Errorf("Expected RowLockWaitsPS 0.5, got %.2f", stats.RowLockWaitsPS)
	}
}

func TestMysqlCollectorCounterReset(t *testing.T) {
	// Simulate a MySQL restart: previous counters are very high,
	// current counters reset to small values.  Rates must be 0, not insane.
	mc := &mysqlCollector{
		prev: mysqlRaw{
			questions:        50_000_000,
			comSelect:        30_000_000,
			comInsert:        5_000_000,
			comUpdate:        2_000_000,
			comDelete:        1_000_000,
			slowQueries:      100_000,
			innodbBPReads:    500_000,
			innodbBPRequests: 5_000_000,
			tableLocksWaited: 50_000,
			rowLockWaits:     10_000,
			valid:            true,
		},
	}

	cur := mysqlRaw{
		valid:            true,
		questions:        100,
		comSelect:        50,
		comInsert:        10,
		comUpdate:        5,
		comDelete:        2,
		slowQueries:      1,
		innodbBPReads:    5,
		innodbBPRequests: 200,
		tableLocksWaited: 0,
		rowLockWaits:     0,
	}

	stats := &MysqlStats{}
	mc.calculateStats(stats, cur, 1.0)

	// All rates must be zero (counter rollback guard)
	if stats.QueriesPS != 0 || stats.ComSelectPS != 0 || stats.ComInsertPS != 0 ||
		stats.ComUpdatePS != 0 || stats.ComDeletePS != 0 || stats.SlowQueriesPS != 0 ||
		stats.InnodbBPReadsPS != 0 || stats.TableLocksWaitedPS != 0 || stats.RowLockWaitsPS != 0 {
		t.Errorf("Expected all rates to be 0 after counter reset, got: q=%.2f sel=%.2f ins=%.2f upd=%.2f del=%.2f slow=%.2f bpReads=%.2f tblLk=%.2f rowLk=%.2f",
			stats.QueriesPS, stats.ComSelectPS, stats.ComInsertPS, stats.ComUpdatePS,
			stats.ComDeletePS, stats.SlowQueriesPS, stats.InnodbBPReadsPS,
			stats.TableLocksWaitedPS, stats.RowLockWaitsPS)
	}
}

// TestMysqlHitRatioWithoutMeaning: the InnoDB buffer pool hit ratio is left at
// 0, as before the first read request, when the server counts more disk reads
// than read requests (MariaDB 11.8 does: it read -678.95 %).
func TestMysqlHitRatioWithoutMeaning(t *testing.T) {
	for _, tc := range []struct {
		name            string
		reads, requests int64
		want            float64
	}{
		{"no read requests yet", 0, 0, 0},
		{"every request from memory", 0, 1000, 100},
		{"every request from disk", 1000, 1000, 0},
		{"more disk reads than requests", 7_890, 1_016, 0},
	} {
		stats := &MysqlStats{}
		(&mysqlCollector{}).calculateStats(stats, mysqlRaw{innodbBPReads: tc.reads, innodbBPRequests: tc.requests, valid: true}, 0)
		if stats.InnodbBufferPoolHitPct != tc.want {
			t.Errorf("%s: hit ratio %.2f %%, want %.2f", tc.name, stats.InnodbBufferPoolHitPct, tc.want)
		}
	}
}

// TestMysqlRatesNeedABaseline: a failed poll leaves no baseline, so the next
// poll reports no rates instead of two intervals of growth over one; a
// server with no queries yet still establishes one.
func TestMysqlRatesNeedABaseline(t *testing.T) {
	mc := &mysqlCollector{}
	poll := func(questions int64) *MysqlStats {
		t.Helper()
		stats := &MysqlStats{}
		mc.calculateStats(stats, mysqlRaw{questions: questions, valid: true}, 1)
		return stats
	}
	poll(0)
	if s := poll(10); s.QueriesPS != 10 {
		t.Fatalf("from a zero baseline: %+v, want 10 q/s", s)
	}
	mc.prev = mysqlRaw{} // what a failed poll leaves
	if s := poll(30); s.QueriesPS != 0 {
		t.Fatalf("after a failed poll: %+v, want no rate", s)
	}
	if s := poll(35); s.QueriesPS != 5 {
		t.Fatalf("baseline restored: %+v, want 5 q/s", s)
	}
}

// TestCustomSocketPathTooLong: a socket path longer than a Unix socket can be
// bound at is refused with the path, its length, the limit and the setting to
// change, not the kernel's bare "invalid argument"; one at the limit listens.
func TestCustomSocketPathTooLong(t *testing.T) {
	t.Chdir(t.TempDir()) // relative paths: their length does not depend on TMPDIR
	cfg := map[string][]config.CustomMetricConfig{"fans": {{Name: "fan1", Unit: "RPM", Max: 5000}}}
	socketPath := func(length int) string {
		dir := strings.Repeat("d", length-len("/kula.sock"))
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(dir, "kula.sock")
	}

	cc, err := newCustomCollector(context.Background(), socketPath(107), cfg, time.Minute, false)
	if err != nil {
		t.Fatalf("a 107-byte socket path: %v", err)
	}
	cc.Close()

	long := socketPath(108)
	if cc, err := newCustomCollector(context.Background(), long, cfg, time.Minute, false); err == nil {
		cc.Close()
		t.Fatal("a 108-byte socket path was accepted")
	} else {
		for _, want := range []string{long, "108 bytes", "107", "storage.directory", "at most 97 bytes"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	}
}

func TestCustomCollector(t *testing.T) {
	sockPath := "/tmp/kula_test.sock"
	defer func() { _ = os.Remove(sockPath) }()

	cfg := map[string][]config.CustomMetricConfig{
		"fans": {
			{Name: "fan1", Unit: "RPM", Max: 5000},
		},
	}

	cc, err := newCustomCollector(context.Background(), sockPath, cfg, time.Minute, false)
	if err != nil {
		t.Fatalf("Failed to create custom collector: %v", err)
	}
	defer cc.Close()

	// Send valid metric
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("Failed to connect to socket: %v", err)
	}

	msg := map[string]any{
		"custom": map[string]any{
			"fans": []map[string]any{
				{"fan1": 1234},
			},
		},
	}
	_ = json.NewEncoder(conn).Encode(msg)
	_ = conn.Close()

	// Busy wait for processing (since acceptLoop is async)
	var latest map[string][]CustomMetricValue
	for i := 0; i < 20; i++ {
		latest = cc.Latest()
		if len(latest) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(latest["fans"]) == 0 {
		t.Fatal("No metrics received")
	}
	if latest["fans"][0].Value != 1234 {
		t.Errorf("Expected 1234, got %.2f", latest["fans"][0].Value)
	}
}

// A second collector on the same path must not take over the socket a
// running collector (another kula process) serves.
func TestCustomCollectorKeepsLiveSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "kula.sock")
	cfg := map[string][]config.CustomMetricConfig{"fans": {{Name: "fan1"}}}

	first, err := newCustomCollector(context.Background(), sockPath, cfg, time.Minute, false)
	if err != nil {
		t.Fatalf("first collector: %v", err)
	}
	defer first.Close()

	if second, err := newCustomCollector(context.Background(), sockPath, cfg, time.Minute, false); err == nil {
		second.Close()
		t.Fatal("second collector replaced a live socket")
	}
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("live socket gone after the refused takeover: %v", err)
	}
	_ = conn.Close()

	// A socket file nobody serves is stale and is replaced.
	first.Close()
	stale, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()
	replacement, err := newCustomCollector(context.Background(), sockPath, cfg, time.Minute, false)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	replacement.Close()
}

// TestCustomCollectorDedup verifies that a single message collapses to one value
// per configured name (last writer wins), emits in config order regardless of
// input order, and drops unconfigured names. Without this, a repeated name would
// produce duplicate downstream series (e.g. a rejected Prometheus scrape).
func TestCustomCollectorDedup(t *testing.T) {
	configs := map[string][]config.CustomMetricConfig{
		"fans": {{Name: "fan1"}, {Name: "fan2"}},
	}
	set, order := indexConfigs(configs)
	cc := &customCollector{
		latest:      make(map[string]customGroup),
		configSet:   set,
		configOrder: order,
		staleAfter:  time.Minute,
	}

	cc.processMessage(map[string][]map[string]float64{
		"fans": {
			{"fan2": 800},
			{"fan1": 100},
			{"fan1": 200}, // duplicate of fan1 — must overwrite 100
			{"nope": 999}, // unconfigured — must be dropped
		},
	})

	got := cc.Latest()["fans"]
	want := []CustomMetricValue{
		{Name: "fan1", Value: 200},
		{Name: "fan2", Value: 800},
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d metrics, got %d: %+v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("metric %d: expected %+v, got %+v", i, want[i], got[i])
		}
	}
}

// TestCustomCollectorRefusesUnstorableValues verifies that a value beyond the
// 32-bit float range history stores is left out like a missing reading, that
// the rest of its message is kept, and that the refusal is logged once per
// metric, not on every message.
func TestCustomCollectorRefusesUnstorableValues(t *testing.T) {
	configs := map[string][]config.CustomMetricConfig{
		"fans": {{Name: "fan1"}, {Name: "fan2"}},
	}
	set, order := indexConfigs(configs)
	cc := &customCollector{
		latest:      make(map[string]customGroup),
		configSet:   set,
		configOrder: order,
		staleAfter:  time.Minute,
	}
	var logged strings.Builder
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	values := func() []CustomMetricValue { return cc.Latest()["fans"] }
	only := func(step string, want CustomMetricValue) {
		t.Helper()
		if got := values(); len(got) != 1 || got[0] != want {
			t.Fatalf("%s: got %+v, want only %+v", step, got, want)
		}
	}

	cc.processMessage(map[string][]map[string]float64{"fans": {{"fan1": 1e39}, {"fan2": 800}}})
	only("out of range beside a good value", CustomMetricValue{Name: "fan2", Value: 800})

	// A message with nothing storable leaves the group as it was.
	cc.processMessage(map[string][]map[string]float64{"fans": {{"fan1": -math.MaxFloat64}}})
	only("out of range alone", CustomMetricValue{Name: "fan2", Value: 800})
	if n := strings.Count(logged.String(), "fans/fan1"); n != 1 {
		t.Errorf("refusal logged %d times, want once:\n%s", n, logged.String())
	}

	// The largest float32 is storable, and a metric back in range is no
	// longer remembered as refused.
	cc.processMessage(map[string][]map[string]float64{"fans": {{"fan1": math.MaxFloat32}}})
	only("largest float32", CustomMetricValue{Name: "fan1", Value: math.MaxFloat32})
	if len(cc.outOfRange) != 0 {
		t.Errorf("still marked out of range: %v", cc.outOfRange)
	}
}

// TestCustomCollectorStale verifies that a group's values are dropped once its
// producer stops pushing, so a stopped feed leaves a gap rather than freezing at
// its last value.
func TestCustomCollectorStale(t *testing.T) {
	configs := map[string][]config.CustomMetricConfig{"fans": {{Name: "fan1"}}}
	set, order := indexConfigs(configs)
	cc := &customCollector{
		latest:      make(map[string]customGroup),
		configSet:   set,
		configOrder: order,
		staleAfter:  20 * time.Millisecond,
	}

	cc.processMessage(map[string][]map[string]float64{"fans": {{"fan1": 1234}}})
	if got := cc.Latest(); len(got["fans"]) != 1 {
		t.Fatalf("fresh value should be present, got %+v", got)
	}

	time.Sleep(40 * time.Millisecond) // exceed the staleness window
	if got := cc.Latest(); got != nil {
		t.Errorf("stale value should be dropped, got %+v", got)
	}
}

// failingListener fails every Accept the way a process at its descriptor
// limit does, until it is closed.
type failingListener struct {
	accepts atomic.Int64
	closed  chan struct{}
}

func (l *failingListener) Accept() (net.Conn, error) {
	l.accepts.Add(1)
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	default:
		return nil, &net.OpError{Op: "accept", Net: "unix", Err: syscall.EMFILE}
	}
}

func (l *failingListener) Close() error   { close(l.closed); return nil }
func (l *failingListener) Addr() net.Addr { return &net.UnixAddr{Name: "kula.sock", Net: "unix"} }

// captureLog sends the standard logger's output to a buffer for the test.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	var logged syncBuffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &logged
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// An accept that keeps failing (EMFILE at the descriptor limit) is retried
// with a growing pause and logged once, not in a loop that spins a core and
// writes hundreds of thousands of log lines a second.
func TestCustomAcceptErrorsBackOff(t *testing.T) {
	logged := captureLog(t)
	ln := &failingListener{closed: make(chan struct{})}
	cc := &customCollector{listener: ln, conns: make(chan struct{}, maxCustomConns)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		cc.acceptLoop(ctx)
		close(done)
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	_ = ln.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the accept loop did not stop after its context ended")
	}
	// 5+10+20+40+80+160 ms: about seven attempts fit in 300 ms.
	if n := ln.accepts.Load(); n > 12 {
		t.Errorf("%d accepts in 300 ms; a failing accept must back off", n)
	}
	if n := strings.Count(logged.String(), "accept error"); n != 1 {
		t.Errorf("accept error logged %d times, want once:\n%s", n, logged.String())
	}
}

// The socket serves at most maxCustomConns clients at once: one more is
// disconnected straight away, and logged once, and a client is served again
// as soon as a connection closes.
func TestCustomCollectorCapsConnections(t *testing.T) {
	logged := captureLog(t)
	sockPath := filepath.Join(t.TempDir(), "kula.sock")
	cc, err := newCustomCollector(context.Background(), sockPath, map[string][]config.CustomMetricConfig{"fans": {{Name: "fan1"}}}, time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	held := make([]net.Conn, 0, maxCustomConns)
	defer func() {
		for _, conn := range held {
			_ = conn.Close()
		}
	}()
	for range maxCustomConns {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting until %s", what)
			}
		}
	}
	waitFor("every idle connection is served", func() bool { return len(cc.conns) == maxCustomConns })

	refused := func() {
		t.Helper()
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("a connection past the cap was not closed at once: read error %v", err)
		}
	}
	refused()
	refused()
	if n := strings.Count(logged.String(), "connections already open"); n != 1 {
		t.Errorf("refusals logged %d times, want once:\n%s", n, logged.String())
	}

	_ = held[0].Close()
	held = held[1:]
	waitFor("a closed connection frees its slot", func() bool { return len(cc.conns) < maxCustomConns })
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte(`{"custom":{"fans":[{"fan1":42}]}}` + "\n"))
	_ = conn.Close()
	waitFor("the freed slot serves a producer", func() bool { return len(cc.Latest()["fans"]) == 1 })
}

// A client's bytes are quoted in the log: a decoding error may name the map
// keys it failed on, control characters and all, and a valid line may carry a
// raw carriage return between its tokens.
func TestCustomLinesAreLoggedEscaped(t *testing.T) {
	logged := captureLog(t)
	cc := &customCollector{latest: make(map[string]customGroup), debug: true}
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		cc.handleConn(server)
		close(done)
	}()
	const rawKey = "\x1b]0;owned\a"
	badValue := "{\"custom\":{\"\\u001b]0;owned\\u0007\":[{\"a\":\"s\"}]}}"
	_, _ = client.Write([]byte(badValue + "\n"))
	_, _ = client.Write([]byte("{\"custom\":\r{\"g\":[{\"a\":1}]}}\n"))
	_ = client.Close()
	<-done
	out := logged.String()
	if strings.ContainsAny(out, "\x1b\a\r") {
		t.Errorf("the log carries raw control characters: %q", out)
	}
	if !strings.Contains(out, "invalid JSON") || !strings.Contains(out, `\r{`) {
		t.Errorf("the log does not show the rejected line and the escaped valid one: %q", out)
	}
	// encoding/json names the map key in its error from Go 1.27 on; an older
	// decoder names only the struct field, leaving no client bytes to escape.
	err := json.Unmarshal([]byte(badValue), new(customMessage))
	if err == nil {
		t.Fatal("a string where a number belongs was decoded")
	}
	if strings.Contains(err.Error(), rawKey) && !strings.Contains(out, `\x1b]0;owned\a`) {
		t.Errorf("the log does not show the escaped map key: %q", out)
	}
}

func TestDefaultCustomStaleAfter(t *testing.T) {
	cases := []struct {
		interval time.Duration
		want     time.Duration
	}{
		{time.Second, 5 * time.Second},            // default interval -> 5 cycles
		{2 * time.Second, 10 * time.Second},       // scales with interval
		{100 * time.Millisecond, 5 * time.Second}, // sub-second clamps to the floor
		{0, 5 * time.Second},                      // misconfigured interval clamps to the floor
	}
	for _, c := range cases {
		if got := defaultCustomStaleAfter(c.interval); got != c.want {
			t.Errorf("interval %s: want %s, got %s", c.interval, c.want, got)
		}
	}
}

// TestStatusPagesPollOffTheCollectionTick: nginx and Apache2 status pages are
// polled in their own goroutines, so a server that stops answering delays only
// its own readings. Collect returns at once and reports the page missing until
// a poll completes, then reports what that poll read.
func TestStatusPagesPollOffTheCollectionTick(t *testing.T) {
	release := make(chan struct{})
	pages := map[string]string{
		"/nginx":   "Active connections: 3\nserver accepts handled requests\n 10 10 20\nReading: 0 Writing: 1 Waiting: 2\n",
		"/apache2": "Total Accesses: 100\nTotal kBytes: 50\nBusyWorkers: 2\nIdleWorkers: 8\nScoreboard: __WR\n",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = w.Write([]byte(pages[r.URL.Path]))
	}))
	defer server.Close()
	defer close(release)

	c := New(config.GlobalConfig{}, config.CollectionConfig{Interval: time.Second}, config.ApplicationsConfig{
		Nginx:   config.NginxConfig{Enabled: true, StatusURL: server.URL + "/nginx"},
		Apache2: config.Apache2Config{Enabled: true, StatusURL: server.URL + "/apache2"},
	}, t.TempDir())
	c.StartApplications()
	defer c.Stop()

	started := time.Now()
	s := c.Collect()
	if took := time.Since(started); took > 500*time.Millisecond {
		t.Fatalf("Collect waited %v on unresponsive status pages", took)
	}
	if s.Apps.Nginx != nil || s.Apps.Apache2 != nil {
		t.Fatalf("Collect reported status pages no poll has read: %+v %+v", s.Apps.Nginx, s.Apps.Apache2)
	}

	release <- struct{}{}
	release <- struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for s = c.Collect(); s.Apps.Nginx == nil || s.Apps.Apache2 == nil; s = c.Collect() {
		if time.Now().After(deadline) {
			t.Fatal("status page polls never completed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s.Apps.Nginx.ActiveConnections != 3 || s.Apps.Apache2.TotalAccesses != 100 {
		t.Fatalf("Collect reported %+v and %+v", s.Apps.Nginx, s.Apps.Apache2)
	}
}

// A status page that keeps failing is logged when its error changes and once
// when it answers again, not on every poll and not only during the first
// collection cycle.
func TestPollLogReportsChanges(t *testing.T) {
	var logs strings.Builder
	orig := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(orig)

	l := pollLog{name: "[nginx]"}
	l.ok()
	for range 3 {
		l.failf("unexpected status: %d", 502)
	}
	l.failf("fetch error: %s", "connection refused")
	l.ok()
	l.ok()
	l.failf("unexpected status: %d", 502)

	got := logs.String()
	for msg, want := range map[string]int{
		"[nginx] unexpected status: 502":          2,
		"[nginx] fetch error: connection refused": 1,
		"[nginx] status page answering again":     1,
	} {
		if n := strings.Count(got, msg); n != want {
			t.Errorf("%q logged %d times, want %d:\n%s", msg, n, want, got)
		}
	}
}
