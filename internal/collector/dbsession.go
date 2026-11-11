package collector

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// ---- Database sessions -------------------------------------------------------
//
// The PostgreSQL and MySQL collectors must never be why a server runs out of
// connections. Each owns at most one session and keeps these rules:
//
//   - One pinned connection, taken from a pool capped at one, so the pool
//     never opens a second connection behind the collector's back.
//   - Polls run in their own goroutine, one at a time. A slow poll delays the
//     next one; it is not cut short at the collection interval, which would
//     leave the server running a query for a client that has gone, while the
//     next tick opened another session beside it.
//   - The server bounds the session by itself: statement and lock timeouts end
//     runaway queries, idle and keepalive timeouts end a session whose client
//     vanished (network loss, host crash) instead of the hours the operating
//     system's defaults allow.
//   - A session the collector gives up on — its poll reached the hard timeout,
//     or its connection broke mid-query — is remembered. The next connection
//     first terminates it, matched by a server-unique identity so no other
//     client's session is touched. While it is still alive the new connection
//     closes again and the collector backs off instead of polling beside it,
//     so at most two collector sessions ever exist, one of them dying.
//   - Failed connections and polls back off exponentially, so a server that
//     refuses connections ("too many clients") is not retried every second.
//   - A driver panic on a malformed reply fails that poll alone (poller.run):
//     the session it interrupted is discarded, remembered for reaping, and
//     the collector backs off.
// ------------------------------------------------------------------------------

// dbSessionRef identifies one server session uniquely: PostgreSQL by backend
// pid and start time, MySQL by connection id, user and server start time.
type dbSessionRef struct {
	id      int64
	user    string
	started string
}

// dbDialect supplies the database-specific parts of a session.
type dbDialect struct {
	// setup applies session settings to a new connection and returns its
	// server identity.
	setup func(ctx context.Context, conn *sql.Conn) (dbSessionRef, error)
	// reap terminates an abandoned session if it still exists, and reports
	// whether it is gone.
	reap func(ctx context.Context, conn *sql.Conn, ref dbSessionRef) (bool, error)
	// describe names a session in log lines.
	describe func(ref dbSessionRef) string
	// abort, when set, closes every network connection the collector holds.
	// It is the last resort for a driver that keeps reading after its context
	// ends (lib/pq), see dbSession.bound.
	abort func()
	// shut and reopen, when set, end what a driver panic left behind (see
	// dbSession.discard): shut closes every network connection like abort and
	// refuses new ones until reopen, which the next connection attempt calls.
	shut, reopen func()
}

// dbTimeouts derives every session bound from the collection interval.
type dbTimeouts struct {
	// poll is the hard limit for one poll; reaching it abandons the session.
	poll time.Duration
	// statement and lock are the server-side query limits, well inside poll.
	statement time.Duration
	lock      time.Duration
	// idle ends a session the server has not heard from in this long. It
	// comfortably exceeds the gap between two polls.
	idle time.Duration
	// stale is how long a poll's result stays current.
	stale time.Duration
	// backoffCap bounds the wait between failed attempts.
	backoffCap time.Duration
	// abortGrace is how long work may outlive its deadline before the
	// dialect's abort cuts its connections.
	abortGrace time.Duration
	interval   time.Duration
}

func newDBTimeouts(interval time.Duration) dbTimeouts {
	if interval <= 0 {
		interval = time.Second
	}
	poll := min(max(10*time.Second, 2*interval), time.Minute)
	return dbTimeouts{
		poll:       poll,
		statement:  poll / 2,
		lock:       2 * time.Second,
		idle:       max(time.Minute, 4*interval),
		stale:      pollStale(interval),
		backoffCap: max(5*time.Minute, interval),
		abortGrace: 5 * time.Second,
		interval:   interval,
	}
}

var (
	errDBBackoff          = errors.New("waiting before the next connection attempt")
	errDBAbandonedSession = errors.New("a session given up on earlier is still active")
)

// dbSession owns the single session of one database collector. It is used
// only from the collector's poll goroutine.
type dbSession struct {
	name      string // log prefix, e.g. "[postgres]"
	label     string // database shown in log lines
	connector driver.Connector
	dialect   dbDialect
	timeouts  dbTimeouts
	now       func() time.Time
	logf      func(format string, args ...any) // nil: the standard logger

	db        *sql.DB
	conn      *sql.Conn
	self      dbSessionRef
	hasSelf   bool
	abandoned []dbSessionRef

	failures  int
	retryAt   time.Time
	connected bool
	lastErr   string
}

// maxAbandonedSessions bounds the remembered sessions. Reaching it would take
// several sessions that refused to die; keeping the newest is enough, because
// no new poll runs until every remembered one is gone.
const maxAbandonedSessions = 4

// bound limits ctx to d. A driver that honours its context returns by then.
// One that does not — lib/pq marks the connection bad, sends a cancel request
// and keeps reading until the server answers — can stay blocked far longer: a
// frozen server whose kernel still acknowledges TCP (a paused container), or
// unacknowledged data waiting on the retransmission timer (about 15 minutes),
// is something keepalives cannot detect. So when the dialect can abort, a
// timer cuts its connections once the work has outlived d by abortGrace,
// which ends the blocked read at once. The returned cancel disarms it.
func (s *dbSession) bound(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, d)
	if s.dialect.abort == nil {
		return ctx, cancel
	}
	timer := time.AfterFunc(d+s.timeouts.abortGrace, s.dialect.abort)
	return ctx, func() {
		timer.Stop()
		cancel()
	}
}

func (s *dbSession) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *dbSession) printf(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// acquire returns the session's connection, connecting when needed and
// reaping abandoned sessions first.
func (s *dbSession) acquire(ctx context.Context) (*sql.Conn, error) {
	if s.clock().Before(s.retryAt) {
		return nil, errDBBackoff
	}
	if s.conn == nil {
		if err := s.open(ctx); err != nil {
			if ctx.Err() != nil {
				return nil, err // shutting down, not a failed connection
			}
			s.backoff()
			s.logOnce(fmt.Sprintf("%s cannot connect to %q: %v", s.name, s.label, err))
			return nil, err
		}
	}
	if err := s.reapAbandoned(ctx); err != nil {
		// Never poll beside a session that may still be working: close this
		// connection again and wait.
		s.release()
		s.backoff()
		return nil, err
	}
	return s.conn, nil
}

// open connects. The pool and the connection are the session's as soon as
// they exist, so a driver panic while connecting leaves them where discard
// finds them.
func (s *dbSession) open(ctx context.Context) error {
	if s.dialect.reopen != nil {
		s.dialect.reopen()
	}
	s.db = sql.OpenDB(s.connector)
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	// The pinned connection never returns to the pool, so lifetime limits
	// would not apply to it; the session's own server-side timeouts do.
	s.db.SetConnMaxLifetime(0)

	connectCtx, cancel := s.bound(ctx, s.timeouts.poll)
	defer cancel()
	conn, err := s.db.Conn(connectCtx)
	if err != nil {
		s.release()
		return err
	}
	s.conn = conn
	ref, err := s.dialect.setup(connectCtx, conn)
	if err != nil {
		s.release()
		return fmt.Errorf("session setup: %w", err)
	}
	s.self, s.hasSelf = ref, true
	if !s.connected {
		s.printf("%s connected to %q (%s)", s.name, s.label, s.dialect.describe(ref))
		s.connected = true
	}
	s.lastErr = ""
	return nil
}

// reapAbandoned terminates every remembered session through the current
// connection. It fails while any of them is still alive.
func (s *dbSession) reapAbandoned(ctx context.Context) error {
	if len(s.abandoned) == 0 {
		return nil
	}
	reapCtx, cancel := s.bound(ctx, s.timeouts.poll)
	defer cancel()
	remaining := s.abandoned[:0]
	var firstErr error
	for _, ref := range s.abandoned {
		gone, err := s.dialect.reap(reapCtx, s.conn, ref)
		switch {
		case err != nil:
			remaining = append(remaining, ref)
			if firstErr == nil {
				firstErr = fmt.Errorf("terminating %s: %w", s.dialect.describe(ref), err)
			}
		case gone:
			s.printf("%s ended abandoned session %s", s.name, s.dialect.describe(ref))
		default:
			remaining = append(remaining, ref)
			if firstErr == nil {
				firstErr = fmt.Errorf("%w (%s)", errDBAbandonedSession, s.dialect.describe(ref))
			}
		}
	}
	s.abandoned = remaining
	if firstErr != nil {
		s.logOnce(fmt.Sprintf("%s not polling %q yet: %v", s.name, s.label, firstErr))
	}
	return firstErr
}

// succeed records a completed poll, and says so after a logged failure: a
// connection lost, or a poll the server refused on a healthy session.
func (s *dbSession) succeed() {
	if s.failures > 0 || s.lastErr != "" {
		s.printf("%s polling %q again", s.name, s.label)
	}
	s.failures = 0
	s.retryAt = time.Time{}
	s.lastErr = ""
}

// fail handles a poll that ended in err. When the connection still answers,
// the session is intact (the server rejected one statement) and stays. When it
// does not — a timeout, or a connection broken mid-query — the server may
// still be running that session: it is remembered for reaping, the
// connection is closed, and the collector backs off. A poll that shutdown
// (ctx) cut short only closes the connection: the server is not at fault,
// and closing it ends the session.
func (s *dbSession) fail(ctx context.Context, err error) {
	if ctx.Err() != nil {
		s.release()
		return
	}
	if s.conn != nil {
		if s.ping(ctx) == nil && ctx.Err() == nil {
			s.logOnce(fmt.Sprintf("%s poll of %q failed: %v", s.name, s.label, err))
			return
		}
	}
	if s.hasSelf {
		s.printf("%s giving up on session %s after: %v; it is ended before polling again", s.name, s.dialect.describe(s.self), err)
		s.abandon()
	}
	if s.connected {
		s.printf("%s connection to %q lost, will retry", s.name, s.label)
		s.connected = false
	}
	s.release()
	s.backoff()
}

// ping checks that the connection still answers. Its bound is disarmed by a
// deferred cancel, so a driver panic cannot leave the abort timer armed to cut
// the next session's connection.
func (s *dbSession) ping(ctx context.Context) error {
	pingCtx, cancel := s.bound(ctx, min(s.timeouts.poll, 5*time.Second))
	defer cancel()
	return s.conn.PingContext(pingCtx)
}

// backoff schedules the next attempt: one interval after the first failure,
// doubling up to the cap.
func (s *dbSession) backoff() {
	s.failures++
	delay := s.timeouts.interval
	for i := 1; i < s.failures && delay < s.timeouts.backoffCap; i++ {
		delay *= 2
	}
	s.retryAt = s.clock().Add(min(delay, s.timeouts.backoffCap))
}

// release closes the session's connection and pool. Closing an idle
// connection ends its server session cleanly.
func (s *dbSession) release() {
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
	s.hasSelf = false
}

// abandon remembers the current session for reaping: the server may still be
// running it.
func (s *dbSession) abandon() {
	s.abandoned = append(s.abandoned, s.self)
	if len(s.abandoned) > maxAbandonedSessions {
		s.abandoned = s.abandoned[len(s.abandoned)-maxAbandonedSessions:]
	}
}

// discard drops a session a driver panic left in an unknown state, without
// waiting on database/sql: it releases a connection's lock only when the
// statement on it returns, so after a panic mid-statement closing the
// connection the usual way would block forever. Instead the dialect's shut
// closes the network connections, which ends the session on the server, and
// refuses new ones until the next attempt: lib/pq answers the end of the
// interrupted query's context by dialing the server from a goroutine of its
// own to cancel the query, and whoever sent the malformed reply could hold
// that connection, and the goroutine, open forever. The driver connection is
// closed directly (ending the driver's own goroutines), and the pool is
// closed. Like a session given up on, it is remembered for reaping, and the
// collector backs off: a server whose replies crash the driver is not retried
// every interval.
func (s *dbSession) discard() {
	switch {
	case s.dialect.shut != nil:
		s.dialect.shut()
	case s.dialect.abort != nil:
		s.dialect.abort()
	}
	if s.conn != nil {
		// Raw shares the connection lock the panic may have left held. A
		// panic in f would make Raw close the connection, which needs that
		// lock exclusively, so f never lets one out.
		_ = s.conn.Raw(func(dc any) error {
			defer func() { _ = recover() }()
			if c, ok := dc.(driver.Conn); ok {
				_ = c.Close()
			}
			return nil
		})
		s.conn = nil
	}
	if s.db != nil {
		_ = s.db.Close() // closes idle connections only; never waits on the one in use
		s.db = nil
	}
	if s.hasSelf {
		s.abandon()
		s.hasSelf = false
	}
	s.connected = false
	s.backoff()
}

func (s *dbSession) logOnce(msg string) {
	if msg != s.lastErr {
		s.printf("%s", msg)
		s.lastErr = msg
	}
}

// dbDialer dials database connections and keeps track of the ones still open.
// Its keepalives probe an idle connection after 15 s and give up after three
// unanswered probes 5 s apart, so a server that vanished while the session
// sat idle is noticed in seconds instead of the default's minutes. abort
// closes every open connection: dbSession.bound calls it for work stuck past
// its deadline (lib/pq does not close a socket when a query's context ends;
// it sends a cancel request and keeps reading, and keepalives cannot help a
// socket with a read in flight to a frozen server). shut does the same and
// refuses new connections until reopen: dbSession.discard calls it for what a
// driver panic left behind.
type dbDialer struct {
	d     net.Dialer
	mu    sync.Mutex
	conns map[*dbDialerConn]struct{}
	// refusing is set by shut and cleared by reopen.
	refusing bool
}

var errDBDialerShut = errors.New("connection refused: the session was discarded after a driver panic")

// dbDialerConn removes itself from its dialer's open set when closed.
type dbDialerConn struct {
	net.Conn
	owner *dbDialer
	once  sync.Once
}

func (c *dbDialerConn) Close() error {
	c.once.Do(func() {
		c.owner.mu.Lock()
		delete(c.owner.conns, c)
		c.owner.mu.Unlock()
	})
	return c.Conn.Close()
}

func newDBDialer() *dbDialer {
	return &dbDialer{
		d: net.Dialer{KeepAliveConfig: net.KeepAliveConfig{
			Enable: true, Idle: 15 * time.Second, Interval: 5 * time.Second, Count: 3,
		}},
		conns: make(map[*dbDialerConn]struct{}),
	}
}

func (d *dbDialer) track(conn net.Conn, err error) (net.Conn, error) {
	if err != nil {
		return nil, err
	}
	c := &dbDialerConn{Conn: conn, owner: d}
	d.mu.Lock()
	refused := d.refusing
	if !refused {
		d.conns[c] = struct{}{}
	}
	d.mu.Unlock()
	if refused {
		_ = conn.Close()
		return nil, errDBDialerShut
	}
	return c, nil
}

func (d *dbDialer) Dial(network, address string) (net.Conn, error) {
	return d.track(d.d.Dial(network, address))
}

func (d *dbDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.track(d.d.DialContext(ctx, network, address))
}

func (d *dbDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.track(d.d.DialContext(ctx, network, address))
}

// abort closes every connection the dialer opened that is still open,
// failing any read or write blocked on one.
func (d *dbDialer) abort() {
	d.mu.Lock()
	open := make([]*dbDialerConn, 0, len(d.conns))
	for c := range d.conns {
		open = append(open, c)
	}
	d.mu.Unlock()
	for _, c := range open {
		_ = c.Close()
	}
}

// shut closes every open connection, like abort, and fails every connection
// dialed after it, until reopen.
func (d *dbDialer) shut() {
	d.mu.Lock()
	d.refusing = true
	d.mu.Unlock()
	d.abort()
}

// reopen lets connections be dialed again after shut.
func (d *dbDialer) reopen() {
	d.mu.Lock()
	d.refusing = false
	d.mu.Unlock()
}
