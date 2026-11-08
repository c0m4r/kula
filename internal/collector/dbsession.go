package collector

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
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
	// ends (lib/pq): see dbSession.bound.
	abort func()
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
		stale:      max(3*time.Second, 3*interval),
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

func (s *dbSession) open(ctx context.Context) error {
	db := sql.OpenDB(s.connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// The pinned connection never returns to the pool, so lifetime limits
	// would not apply to it; the session's own server-side timeouts do.
	db.SetConnMaxLifetime(0)

	connectCtx, cancel := s.bound(ctx, s.timeouts.poll)
	defer cancel()
	conn, err := db.Conn(connectCtx)
	if err != nil {
		_ = db.Close()
		return err
	}
	ref, err := s.dialect.setup(connectCtx, conn)
	if err != nil {
		_ = conn.Close()
		_ = db.Close()
		return fmt.Errorf("session setup: %w", err)
	}
	s.db, s.conn, s.self, s.hasSelf = db, conn, ref, true
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

// succeed records a completed poll.
func (s *dbSession) succeed() {
	if s.failures > 0 {
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
// connection is closed, and the collector backs off.
func (s *dbSession) fail(ctx context.Context, err error) {
	if s.conn != nil {
		pingCtx, cancel := s.bound(ctx, min(s.timeouts.poll, 5*time.Second))
		pingErr := s.conn.PingContext(pingCtx)
		cancel()
		if pingErr == nil && ctx.Err() == nil {
			s.logOnce(fmt.Sprintf("%s poll of %q failed: %v", s.name, s.label, err))
			return
		}
	}
	if s.hasSelf {
		s.printf("%s giving up on session %s after: %v; it is ended before polling again", s.name, s.dialect.describe(s.self), err)
		s.abandoned = append(s.abandoned, s.self)
		if len(s.abandoned) > maxAbandonedSessions {
			s.abandoned = s.abandoned[len(s.abandoned)-maxAbandonedSessions:]
		}
	}
	if s.connected {
		s.printf("%s connection to %q lost, will retry", s.name, s.label)
		s.connected = false
	}
	s.release()
	s.backoff()
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

func (s *dbSession) logOnce(msg string) {
	if msg != s.lastErr {
		s.printf("%s", msg)
		s.lastErr = msg
	}
}

// dbPoller runs a collector's polls in one goroutine and keeps the latest
// result. A tick that arrives while a poll runs is dropped (time.Ticker
// drops ticks), so polls never overlap. The goroutine alone uses the session;
// cleanup ends it once the context is done.
type dbPoller[T any] struct {
	mu     sync.Mutex
	latest *T
	at     time.Time
	stale  time.Duration
	done   chan struct{}
}

func (p *dbPoller[T]) start(ctx context.Context, interval time.Duration, poll func(context.Context) *T, cleanup func()) {
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		defer cleanup()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			if stats := poll(ctx); stats != nil {
				p.mu.Lock()
				p.latest, p.at = stats, time.Now()
				p.mu.Unlock()
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Latest returns the newest poll result while it is current, nil otherwise.
func (p *dbPoller[T]) Latest() *T {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.latest == nil || time.Since(p.at) > p.stale {
		return nil
	}
	return p.latest
}

// wait blocks until the poll goroutine has exited, up to timeout.
func (p *dbPoller[T]) wait(timeout time.Duration) {
	if p.done == nil {
		return
	}
	select {
	case <-p.done:
	case <-time.After(timeout):
	}
}
