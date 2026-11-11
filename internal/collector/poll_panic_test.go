package collector

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// within runs f and fails the test if it has not returned after d: what it
// guards against is a call that blocks forever.
func within[T any](t *testing.T, d time.Duration, what string, f func() T) T {
	t.Helper()
	done := make(chan T, 1)
	go func() { done <- f() }()
	select {
	case v := <-done:
		return v
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
		panic("unreachable")
	}
}

// TestPollerSurvivesAPanickingPoll: a poll that panics fails alone. The
// poller keeps polling, drops the poll's state through discard, and logs the
// panic, escaped, once per streak: a streak ends only with a poll that returns
// a result. A cleanup that panics is logged too.
func TestPollerSurvivesAPanickingPoll(t *testing.T) {
	logs := captureLog(t)
	var polls, discards atomic.Int32
	p := &poller[int]{name: "[test]", stale: time.Minute, discard: func() { discards.Add(1) }}
	var short []byte
	ctx, cancel := context.WithCancel(context.Background())
	p.start(ctx, time.Millisecond, func(context.Context) *int {
		n := int(polls.Add(1))
		switch {
		case n <= 3:
			_ = short[n] // a driver reading past the end of a short packet
		case n == 4:
			return nil // an ordinary failure: the streak goes on
		case n == 5:
			panic("still the first streak")
		case n == 7:
			panic("pq: unexpected message \x1b]0;owned\a\nforged line")
		}
		return &n
	}, func() { panic("closing") })

	deadline := time.Now().Add(5 * time.Second)
	for polls.Load() < 9 {
		if time.Now().After(deadline) {
			t.Fatalf("the poller stopped after %d polls", polls.Load())
		}
		time.Sleep(time.Millisecond)
	}
	if p.Latest() == nil {
		t.Fatal("no result after the panics")
	}
	cancel()
	p.wait(5 * time.Second)
	select {
	case <-p.done:
	default:
		t.Fatal("the poll goroutine did not end")
	}

	if n := discards.Load(); n != 5 {
		t.Errorf("discard ran %d times, want once for each of the 5 panics", n)
	}
	got := logs.String()
	if n := strings.Count(got, "[test] poll panicked"); n != 2 {
		t.Fatalf("%d panic lines, want one per streak (2):\n%s", n, got)
	}
	for _, want := range []string{
		`"runtime error: index out of range [1] with length 0" at kula/internal/collector.TestPollerSurvivesAPanickingPoll`,
		`"pq: unexpected message \x1b]0;owned\a\nforged line"`,
		`[test] closing panicked: "closing"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b") || strings.Contains(got, "\nforged line") {
		t.Errorf("server text reached the log unescaped:\n%s", got)
	}
}

// panicConnector hands out driver connections whose queries panic, as lib/pq
// and go-sql-driver/mysql do on some malformed server replies.
type panicConnector struct {
	mu    sync.Mutex
	conns []*panicConn
}

func (c *panicConnector) Connect(context.Context) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	conn := &panicConn{}
	c.conns = append(c.conns, conn)
	return conn, nil
}

func (c *panicConnector) Driver() driver.Driver { return fakeDBDriver{} }

type panicConn struct{ closed atomic.Bool }

func (c *panicConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (c *panicConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }
func (c *panicConn) Close() error                        { c.closed.Store(true); return nil }

func (c *panicConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	var short []byte
	return nil, errors.New(string(short[:4]))
}

// TestDBSessionDiscardsAConnectionAPanicLeftLocked: database/sql holds a
// connection's lock while a statement runs and releases it only when the
// statement returns, so a driver panic mid-statement leaves it held and
// closing the connection the usual way blocks forever. discard must return,
// close the driver connection and the network ones, remember the session for
// reaping and back off; the next connection starts clean.
func TestDBSessionDiscardsAConnectionAPanicLeftLocked(t *testing.T) {
	captureLog(t)
	connector := &panicConnector{}
	clock := time.Unix(1_800_000_000, 0)
	var aborts atomic.Int32
	var reaped []dbSessionRef
	s := &dbSession{
		name:      "[fake]",
		label:     "db",
		connector: connector,
		timeouts:  newDBTimeouts(time.Second),
		now:       func() time.Time { return clock },
		logf:      sessionLog(t),
		dialect: dbDialect{
			setup: func(context.Context, *sql.Conn) (dbSessionRef, error) { return dbSessionRef{id: 7}, nil },
			reap: func(_ context.Context, _ *sql.Conn, ref dbSessionRef) (bool, error) {
				reaped = append(reaped, ref)
				return true, nil
			},
			describe: func(dbSessionRef) string { return "fake" },
			abort:    func() { aborts.Add(1) },
		},
	}
	p := &poller[int]{name: "[fake]", discard: s.discard}
	poll := func(ctx context.Context) *int {
		conn, err := s.acquire(ctx)
		if err != nil {
			return nil
		}
		var n int
		_ = conn.QueryRowContext(ctx, "SELECT 1").Scan(&n)
		return &n
	}

	if got := within(t, 10*time.Second, "a poll that panicked", func() *int { return p.run(context.Background(), poll) }); got != nil {
		t.Fatalf("a poll that panicked returned %v", *got)
	}
	if s.conn != nil || s.db != nil {
		t.Fatal("the session a panic left behind is still in use")
	}
	if !connector.conns[0].closed.Load() || aborts.Load() != 1 {
		t.Fatalf("driver connection closed %v, network connections aborted %d times; want true and once",
			connector.conns[0].closed.Load(), aborts.Load())
	}
	if len(s.abandoned) != 1 || s.abandoned[0].id != 7 || s.failures != 1 || !s.retryAt.After(clock) {
		t.Fatalf("abandoned %v, failures %d, retry at %s; want the session remembered and a backoff",
			s.abandoned, s.failures, s.retryAt)
	}

	clock = s.retryAt
	within(t, 10*time.Second, "a second poll", func() *int { return p.run(context.Background(), poll) })
	if len(connector.conns) != 2 || len(reaped) != 1 || reaped[0].id != 7 {
		t.Fatalf("%d connections, reaped %v; want a fresh connection that reaped the first session", len(connector.conns), reaped)
	}
	within(t, 10*time.Second, "release", func() bool { s.release(); return true })
}

// pingPanicConnector hands out connections whose queries fail and whose
// ping panics.
type pingPanicConnector struct{}

func (pingPanicConnector) Connect(context.Context) (driver.Conn, error) { return &pingPanicConn{}, nil }
func (pingPanicConnector) Driver() driver.Driver                        { return fakeDBDriver{} }

type pingPanicConn struct{ panicConn }

func (c *pingPanicConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return nil, errors.New("query failed")
}

func (c *pingPanicConn) Ping(context.Context) error {
	var short []byte
	return errors.New(string(short[:4]))
}

// TestDBSessionPingPanicLeavesNoAbortArmed: a failed poll pings the
// connection under a bound whose timer aborts every connection once the ping
// outlives it. A ping that panics must disarm that timer on the way out, or
// it fires later and cuts whatever connection the collector holds by then.
func TestDBSessionPingPanicLeavesNoAbortArmed(t *testing.T) {
	captureLog(t)
	var aborts atomic.Int32
	s := &dbSession{
		name:      "[fake]",
		label:     "db",
		connector: pingPanicConnector{},
		timeouts: dbTimeouts{
			poll: 20 * time.Millisecond, abortGrace: 20 * time.Millisecond,
			interval: time.Second, backoffCap: time.Minute,
		},
		logf: sessionLog(t),
		dialect: dbDialect{
			setup:    func(context.Context, *sql.Conn) (dbSessionRef, error) { return dbSessionRef{id: 7}, nil },
			reap:     func(context.Context, *sql.Conn, dbSessionRef) (bool, error) { return true, nil },
			describe: func(dbSessionRef) string { return "fake" },
			abort:    func() { aborts.Add(1) },
		},
	}
	p := &poller[int]{name: "[fake]", discard: s.discard}
	poll := func(ctx context.Context) *int {
		conn, err := s.acquire(ctx)
		if err != nil {
			return nil
		}
		var n int
		if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&n); err != nil {
			s.fail(ctx, err)
			return nil
		}
		return &n
	}
	if got := within(t, 10*time.Second, "a poll whose ping panicked", func() *int {
		return p.run(context.Background(), poll)
	}); got != nil {
		t.Fatalf("a poll whose ping panicked returned %v", *got)
	}
	time.Sleep(200 * time.Millisecond) // well past the ping's bound and its grace
	if n := aborts.Load(); n != 1 {
		t.Fatalf("connections aborted %d times; want once, by discard", n)
	}
}

// fakeDBListener accepts connections on loopback and answers each with
// serve. A connection serve reports as a session is kept until the client
// closes it; the listener counts those sessions, and signals each close.
type fakeDBListener struct {
	ln       net.Listener
	sessions atomic.Int32
	closed   chan struct{}
	wg       sync.WaitGroup
}

func startFakeDB(t *testing.T, serve func(conn net.Conn) (session bool)) *fakeDBListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDBListener{ln: ln, closed: make(chan struct{}, 64)}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
				if !serve(conn) {
					return
				}
				f.sessions.Add(1)
				// Wait for the client to close its end.
				if _, err := io.Copy(io.Discard, conn); !errors.Is(err, os.ErrDeadlineExceeded) {
					f.closed <- struct{}{}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		f.wg.Wait()
	})
	return f
}

func (f *fakeDBListener) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

// clientClosed waits for the client to close one of its connections.
func (f *fakeDBListener) clientClosed(t *testing.T) {
	t.Helper()
	select {
	case <-f.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the collector left its connection to the server open")
	}
}

// pgMessage frames a PostgreSQL backend message.
func pgMessage(kind byte, body ...byte) []byte {
	msg := []byte{kind, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(msg[1:], uint32(4+len(body)))
	return append(msg, body...)
}

// readPgStartup reads the client's startup message and reports whether it
// opens a session: lib/pq also connects to send a cancel request.
func readPgStartup(conn net.Conn) bool {
	var size [4]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return false
	}
	msg := make([]byte, int(binary.BigEndian.Uint32(size[:]))-4)
	if _, err := io.ReadFull(conn, msg); err != nil || len(msg) < 4 {
		return false
	}
	const cancelRequest = 80877102
	return binary.BigEndian.Uint32(msg) != cancelRequest
}

// servePgEmptyRowDescription accepts the session and answers its first query
// with a row description that lacks its field count, which panics lib/pq
// reading it.
func servePgEmptyRowDescription(conn net.Conn) {
	var reply []byte
	reply = append(reply, pgMessage('R', 0, 0, 0, 0)...)             // AuthenticationOk
	reply = append(reply, pgMessage('K', 0, 0, 0, 1, 0, 0, 0, 2)...) // BackendKeyData
	reply = append(reply, pgMessage('Z', 'I')...)                    // ReadyForQuery
	if _, err := conn.Write(reply); err != nil {
		return
	}
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}
	if _, err := io.CopyN(io.Discard, conn, int64(binary.BigEndian.Uint32(header[1:]))-4); err != nil {
		return
	}
	_, _ = conn.Write(pgMessage('T')) // RowDescription without its field count
}

// TestPostgresSurvivesAMalformedServer drives the real driver against
// malformed replies: the audit's authentication request with no body, which
// panics lib/pq in readBuf.int32 while it connects, and an empty row
// description, which panics it in the first query with database/sql's
// connection lock held. Either fails the poll, not the process: the
// connection is closed, the next attempt waits for the backoff, and the
// poller still stops cleanly.
func TestPostgresSurvivesAMalformedServer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		serve func(conn net.Conn) bool
		site  string
	}{
		{"startup", func(conn net.Conn) bool {
			if !readPgStartup(conn) {
				return false
			}
			_, _ = conn.Write(pgMessage('R')) // an authentication request without its type
			return true
		}, "github.com/lib/pq.(*readBuf).int32"},
		{"query", func(conn net.Conn) bool {
			if !readPgStartup(conn) {
				return false
			}
			servePgEmptyRowDescription(conn)
			return true
		}, "github.com/lib/pq.(*readBuf).int16"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLog(t)
			srv := startFakeDB(t, tc.serve)
			pc := newPostgresCollector("127.0.0.1", srv.port(), "kula", "secret", "postgres", "disable", false, time.Second)
			clock := time.Now()
			pc.session.now = func() time.Time { return clock }

			for attempt := 1; attempt <= 2; attempt++ {
				if got := within(t, 30*time.Second, "a poll of a malformed server", func() *PostgresStats {
					return pc.poller.run(context.Background(), pc.poll)
				}); got != nil {
					t.Fatalf("poll %d of a malformed server returned %+v", attempt, got)
				}
				srv.clientClosed(t)
				if n := srv.sessions.Load(); n != int32(attempt) {
					t.Fatalf("%d connections after poll %d", n, attempt)
				}
				if got := pc.poller.run(context.Background(), pc.poll); got != nil || srv.sessions.Load() != int32(attempt) {
					t.Fatalf("a poll during the backoff connected or returned %+v", got)
				}
				clock = pc.session.retryAt
			}
			if n := strings.Count(logs.String(), "[postgres] poll panicked"); n != 1 {
				t.Fatalf("%d panic lines for one streak:\n%s", n, logs.String())
			}
			if !strings.Contains(logs.String(), tc.site) {
				t.Fatalf("the panic was not at %s:\n%s", tc.site, logs.String())
			}

			// The real poll goroutine survives it too and stops cleanly.
			ctx, cancel := context.WithCancel(context.Background())
			pc.session.now = nil
			pc.session.retryAt = time.Time{}
			pc.Start(ctx)
			srv.clientClosed(t)
			cancel()
			within(t, 10*time.Second, "Close", func() bool { pc.Close(); return true })
			select {
			case <-pc.poller.done:
			default:
				t.Fatal("the poll goroutine did not end")
			}
		})
	}
}

// pqCancelGoroutines counts lib/pq goroutines sending a cancel request.
func pqCancelGoroutines() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "github.com/lib/pq.(*conn).cancel(")
		}
		buf = make([]byte, 2*len(buf))
	}
}

// TestPostgresPanicLeavesNoConnectionBehind: once a query panics lib/pq, the
// end of its context makes lib/pq dial the server from a goroutine of its own
// to cancel the query, and reads that connection until the server closes it.
// A server that never does must not keep the connection, or the goroutine,
// alive: discarding the session closes its connections and refuses new ones
// until the next attempt, which still connects.
func TestPostgresPanicLeavesNoConnectionBehind(t *testing.T) {
	logs := captureLog(t)
	var open, sessions, cancels atomic.Int32
	srv := startFakeDB(t, func(conn net.Conn) bool {
		open.Add(1)
		defer open.Add(-1)
		if !readPgStartup(conn) {
			cancels.Add(1)
		} else {
			sessions.Add(1)
			servePgEmptyRowDescription(conn)
		}
		_, _ = io.Copy(io.Discard, conn) // hold the connection until the client closes it
		return false
	})
	pc := newPostgresCollector("127.0.0.1", srv.port(), "kula", "secret", "postgres", "disable", false, time.Second)
	clock := time.Now()
	pc.session.now = func() time.Time { return clock }

	const polls = 8
	for range polls {
		if got := within(t, 30*time.Second, "a poll of a malformed server", func() *PostgresStats {
			return pc.poller.run(context.Background(), pc.poll)
		}); got != nil {
			t.Fatalf("a poll of a malformed server returned %+v", got)
		}
		clock = pc.session.retryAt
	}
	deadline := time.Now().Add(5 * time.Second)
	for open.Load() != 0 || pqCancelGoroutines() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("after %d panicking polls: %d connections still open (%d sessions, %d cancel requests seen), %d lib/pq cancel goroutines",
				polls, open.Load(), sessions.Load(), cancels.Load(), pqCancelGoroutines())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := sessions.Load(); n != polls {
		t.Fatalf("%d sessions for %d polls: an attempt after a discarded session did not connect\n%s", n, polls, logs.String())
	}
}

// TestMysqlSurvivesAMalformedServer: the audit's two-byte handshake packet
// panics go-sql-driver/mysql in readHandshakePacket while it connects. The
// poll fails alone and the connection is closed.
func TestMysqlSurvivesAMalformedServer(t *testing.T) {
	logs := captureLog(t)
	srv := startFakeDB(t, func(conn net.Conn) bool {
		_, _ = conn.Write([]byte{2, 0, 0, 0, 10, 0}) // protocol 10, empty version, nothing after
		return true
	})
	mc := newMysqlCollector("127.0.0.1", srv.port(), "kula", "secret", "", "disable", "", false, time.Second)
	if got := within(t, 30*time.Second, "a poll of a malformed server", func() *MysqlStats {
		return mc.poller.run(context.Background(), mc.poll)
	}); got != nil {
		t.Fatalf("a poll of a malformed server returned %+v", got)
	}
	srv.clientClosed(t)
	if mc.session.conn != nil || mc.session.db != nil || mc.session.failures != 1 {
		t.Fatalf("after the panic: connection kept %v, pool kept %v, failures %d",
			mc.session.conn != nil, mc.session.db != nil, mc.session.failures)
	}
	if got := logs.String(); !strings.Contains(got, `[mysql] poll panicked: "runtime error: slice bounds out of range`) ||
		!strings.Contains(got, "github.com/go-sql-driver/mysql.(*mysqlConn).readHandshakePacket") {
		t.Fatalf("the panic in readHandshakePacket was not logged:\n%s", got)
	}
}
