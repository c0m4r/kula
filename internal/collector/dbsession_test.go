package collector

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDBServer models what matters for connection exhaustion: client
// connections the collector holds open, and server sessions, which outlive
// a client connection that was dropped mid-query until something ends them.
type fakeDBServer struct {
	mu         sync.Mutex
	clientOpen int
	clientPeak int
	sessions   map[int64]bool // live server sessions
	sessPeak   int
	nextID     int64
	refuse     bool // new connections are rejected ("too many clients")
	killable   bool // reap can end lingering sessions
	connects   int
}

func newFakeDBServer() *fakeDBServer {
	return &fakeDBServer{sessions: map[int64]bool{}, killable: true}
}

func (s *fakeDBServer) Connect(context.Context) (driver.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connects++
	if s.refuse {
		return nil, errors.New("too many clients already")
	}
	s.nextID++
	s.clientOpen++
	s.clientPeak = max(s.clientPeak, s.clientOpen)
	s.sessions[s.nextID] = true
	s.sessPeak = max(s.sessPeak, len(s.sessions))
	return &fakeDBConn{srv: s, id: s.nextID}, nil
}

func (s *fakeDBServer) Driver() driver.Driver { return fakeDBDriver{} }

func (s *fakeDBServer) state() (clientOpen, sessions int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientOpen, len(s.sessions)
}

type fakeDBDriver struct{}

func (fakeDBDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use the connector") }

type fakeDBConn struct {
	srv    *fakeDBServer
	id     int64
	broken bool // dropped mid-query: closing it leaves the server session running
	closed bool
}

func (c *fakeDBConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (c *fakeDBConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }

func (c *fakeDBConn) Close() error {
	c.srv.mu.Lock()
	defer c.srv.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.srv.clientOpen--
	if !c.broken {
		delete(c.srv.sessions, c.id) // a clean close ends the server session
	}
	return nil
}

func (c *fakeDBConn) Ping(context.Context) error {
	if c.broken {
		return driver.ErrBadConn
	}
	return nil
}

func (c *fakeDBConn) ResetSession(context.Context) error {
	if c.broken {
		return driver.ErrBadConn
	}
	return nil
}

// sessionLogTail is how many of a failed test's session log lines it prints.
const sessionLogTail = 50

// sessionLog keeps a session's log lines out of passing tests: the churn test
// alone writes thousands. A passing test reports how many it held back, a
// failing one prints the last sessionLogTail, and KULA_TEST_LOG=1 sends every
// line to the test log as it is written.
func sessionLog(t *testing.T) func(format string, args ...any) {
	if os.Getenv("KULA_TEST_LOG") != "" {
		return func(format string, args ...any) {
			t.Helper()
			t.Logf(format, args...)
		}
	}
	var mu sync.Mutex
	var lines []string
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case len(lines) == 0:
		case t.Failed():
			tail := lines[max(0, len(lines)-sessionLogTail):]
			t.Logf("session log, last %d of %d lines (KULA_TEST_LOG=1 shows all):\n%s",
				len(tail), len(lines), strings.Join(tail, "\n"))
		default:
			t.Logf("%d session log lines held back (KULA_TEST_LOG=1 shows them)", len(lines))
		}
	})
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
}

// fakeSession builds a dbSession on srv with a controllable clock, logging
// through sessionLog.
func fakeSession(t *testing.T, srv *fakeDBServer, clock *time.Time) *dbSession {
	return &dbSession{
		name:      "[fake]",
		label:     "db",
		connector: srv,
		timeouts:  newDBTimeouts(time.Second),
		now:       func() time.Time { return *clock },
		logf:      sessionLog(t),
		dialect: dbDialect{
			setup: func(_ context.Context, conn *sql.Conn) (dbSessionRef, error) {
				var ref dbSessionRef
				err := conn.Raw(func(dc any) error {
					ref.id = dc.(*fakeDBConn).id
					return nil
				})
				return ref, err
			},
			reap: func(_ context.Context, _ *sql.Conn, ref dbSessionRef) (bool, error) {
				srv.mu.Lock()
				defer srv.mu.Unlock()
				if srv.killable {
					delete(srv.sessions, ref.id)
				}
				return !srv.sessions[ref.id], nil
			},
			describe: func(dbSessionRef) string { return "fake" },
		},
	}
}

// breakConn simulates a poll cut short: the driver dropped the connection,
// the server is still running the session's query.
func breakConn(t *testing.T, conn *sql.Conn) {
	t.Helper()
	if err := conn.Raw(func(dc any) error { dc.(*fakeDBConn).broken = true; return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestDBSessionNeverPollsBesideAnAbandonedSession(t *testing.T) {
	srv := newFakeDBServer()
	clock := time.Unix(1_800_000_000, 0)
	s := fakeSession(t, srv, &clock)
	ctx := context.Background()

	// Steady state: one connection, reused poll after poll.
	conn, err := s.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		again, err := s.acquire(ctx)
		if err != nil || again != conn {
			t.Fatalf("steady acquire: %v, same=%v", err, again == conn)
		}
		s.succeed()
	}
	if srv.connects != 1 {
		t.Fatalf("steady polling opened %d connections, want 1", srv.connects)
	}

	// A server-side error on a healthy session keeps it.
	s.fail(ctx, errors.New("permission denied for pg_stat_statements"))
	if again, err := s.acquire(ctx); err != nil || again != conn || srv.connects != 1 {
		t.Fatalf("server error reconnected: %v connects=%d", err, srv.connects)
	}

	// The poll times out mid-query and the session cannot be killed yet.
	breakConn(t, conn)
	s.fail(ctx, context.DeadlineExceeded)
	if open, sessions := srv.state(); open != 0 || sessions != 1 {
		t.Fatalf("after timeout: %d client connections, %d server sessions; want 0 and 1 lingering", open, sessions)
	}
	if _, err := s.acquire(ctx); !errors.Is(err, errDBBackoff) || srv.connects != 1 {
		t.Fatalf("acquire during backoff: %v connects=%d", err, srv.connects)
	}
	srv.killable = false
	for range 6 {
		clock = clock.Add(10 * time.Minute)
		if _, err := s.acquire(ctx); !errors.Is(err, errDBAbandonedSession) {
			t.Fatalf("acquire beside a live abandoned session: %v", err)
		}
		if open, sessions := srv.state(); open != 0 || sessions != 1 {
			t.Fatalf("probe was kept: %d client connections, %d sessions", open, sessions)
		}
	}

	// Once it can be ended, polling resumes on a fresh session.
	srv.killable = true
	clock = clock.Add(10 * time.Minute)
	conn, err = s.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire after reaping: %v", err)
	}
	if open, sessions := srv.state(); open != 1 || sessions != 1 {
		t.Fatalf("after reaping: %d client connections, %d sessions; want 1 and 1", open, sessions)
	}
	if srv.clientPeak != 1 || srv.sessPeak > 2 {
		t.Fatalf("peaks: %d client connections, %d server sessions; want 1 and at most 2", srv.clientPeak, srv.sessPeak)
	}
	s.succeed()
	_ = conn
	s.release()
	if open, sessions := srv.state(); open != 0 || sessions != 0 {
		t.Fatalf("release left %d client connections, %d sessions", open, sessions)
	}
}

func TestDBSessionBacksOffExponentially(t *testing.T) {
	srv := newFakeDBServer()
	srv.refuse = true
	clock := time.Unix(1_800_000_000, 0)
	s := fakeSession(t, srv, &clock)

	var waits []time.Duration
	for range 12 {
		if _, err := s.acquire(context.Background()); err == nil || errors.Is(err, errDBBackoff) {
			t.Fatalf("acquire against a refusing server: %v", err)
		}
		waits = append(waits, s.retryAt.Sub(clock))
		clock = s.retryAt
	}
	want := []int{1, 2, 4, 8, 16, 32, 64, 128, 256, 300, 300, 300}
	for i, seconds := range want {
		if w := time.Duration(seconds) * time.Second; waits[i] != w {
			t.Fatalf("backoff %d = %s, want %s (all: %v)", i, waits[i], w, waits)
		}
	}
	if srv.connects != 12 {
		t.Fatalf("%d connection attempts over the backoff schedule, want 12", srv.connects)
	}

	srv.refuse = false
	if _, err := s.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.succeed()
	if s.failures != 0 || !s.retryAt.IsZero() {
		t.Fatalf("success left failures=%d retryAt=%s", s.failures, s.retryAt)
	}
}

// TestDBSessionBoundsSessionsUnderChurn drives random timeouts, refusals and
// stuck sessions and checks the invariant that matters: the collector never
// holds more than one client connection or accounts for more than two server
// sessions.
func TestDBSessionBoundsSessionsUnderChurn(t *testing.T) {
	srv := newFakeDBServer()
	clock := time.Unix(1_800_000_000, 0)
	s := fakeSession(t, srv, &clock)
	ctx := context.Background()
	seed := uint64(42)
	random := func(n uint64) uint64 {
		seed = seed*6364136223846793005 + 1442695040888963407
		return (seed >> 33) % n
	}
	for range 5000 {
		srv.mu.Lock()
		srv.refuse = random(10) == 0
		srv.killable = random(3) != 0
		srv.mu.Unlock()
		clock = clock.Add(time.Duration(1+random(120)) * time.Second)
		conn, err := s.acquire(ctx)
		if err != nil {
			continue
		}
		switch random(4) {
		case 0:
			breakConn(t, conn)
			s.fail(ctx, context.DeadlineExceeded)
		case 1:
			s.fail(ctx, errors.New("statement failed"))
		default:
			s.succeed()
		}
		if open, sessions := srv.state(); open > 1 || sessions > 2 {
			t.Fatalf("%d client connections, %d server sessions", open, sessions)
		}
	}
	if srv.clientPeak != 1 || srv.sessPeak > 2 {
		t.Fatalf("peaks: %d client connections, %d server sessions", srv.clientPeak, srv.sessPeak)
	}
}

// TestDBPollerNeverOverlapsPolls: a poll slower than the interval delays the
// next one instead of running beside it, and its result goes stale.
func TestDBPollerNeverOverlapsPolls(t *testing.T) {
	var running, peak, polls atomic.Int32
	p := &dbPoller[int]{stale: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	cleaned := make(chan struct{})
	p.start(ctx, 5*time.Millisecond, func(context.Context) *int {
		n := running.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		running.Add(-1)
		v := int(polls.Add(1))
		return &v
	}, func() { close(cleaned) })

	time.Sleep(200 * time.Millisecond)
	if got := p.Latest(); got == nil {
		t.Fatal("no current result while polling")
	}
	cancel()
	p.wait(time.Second)
	select {
	case <-cleaned:
	default:
		t.Fatal("cleanup did not run when the poller stopped")
	}
	if peak.Load() != 1 {
		t.Fatalf("%d polls ran at once", peak.Load())
	}
	time.Sleep(60 * time.Millisecond)
	if p.Latest() != nil {
		t.Fatal("a stale result is still served")
	}
}

// TestDBSessionBoundAbortsWorkPastItsDeadline pins the last resort for a
// driver that ignores its context: work still running abortGrace after its
// deadline has its connections cut, and work that finished in time never does.
func TestDBSessionBoundAbortsWorkPastItsDeadline(t *testing.T) {
	aborted := make(chan struct{}, 4)
	s := &dbSession{
		timeouts: dbTimeouts{abortGrace: 20 * time.Millisecond},
		dialect:  dbDialect{abort: func() { aborted <- struct{}{} }},
	}

	ctx, cancel := s.bound(context.Background(), 10*time.Millisecond)
	<-ctx.Done()
	select {
	case <-aborted:
	case <-time.After(2 * time.Second):
		t.Fatal("work stuck past its deadline was never aborted")
	}
	cancel()

	_, cancel = s.bound(context.Background(), 10*time.Millisecond)
	cancel()
	select {
	case <-aborted:
		t.Fatal("work that finished in time was aborted")
	case <-time.After(100 * time.Millisecond):
	}

	// Without an abort hook bound is a plain timeout.
	plain := &dbSession{timeouts: dbTimeouts{abortGrace: time.Millisecond}}
	ctx, cancel = plain.bound(context.Background(), time.Millisecond)
	defer cancel()
	<-ctx.Done()
}

// TestPgDialerAbortEndsBlockedReads covers what bound relies on for
// PostgreSQL: abort fails a read blocked on a server that never answers, and
// closed connections leave the dialer's open set.
func TestPgDialerAbortEndsBlockedReads(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var held []net.Conn // accepted, never answered: a frozen server
	accepting := make(chan struct{})
	go func() {
		defer close(accepting)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, conn)
		}
	}()
	defer func() {
		_ = ln.Close()
		<-accepting
		for _, conn := range held {
			_ = conn.Close()
		}
	}()

	d := newPgDialer()
	closed, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := d.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		read <- err
	}()
	select {
	case err := <-read:
		t.Fatalf("read returned before abort: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	d.abort()
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("blocked read succeeded after abort")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("abort did not end the blocked read")
	}
	d.mu.Lock()
	open := len(d.conns)
	d.mu.Unlock()
	if open != 0 {
		t.Fatalf("%d connections still tracked after close and abort", open)
	}
}
