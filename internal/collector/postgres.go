package collector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

// pgRaw holds the raw cumulative counters used to compute per-second rates.
type pgRaw struct {
	xactCommit    int64
	xactRollback  int64
	tupFetched    int64
	tupReturned   int64
	tupInserted   int64
	tupUpdated    int64
	tupDeleted    int64
	blksRead      int64
	blksHit       int64
	deadlocks     int64
	bufCheckpoint int64
	bufBackend    int64
	// valid marks the pg_stat_database counters as read from the server and
	// bgValid the background-writer pair, which is queried separately and
	// can fail on its own (privileges, version). A rate needs both ends.
	valid   bool
	bgValid bool
}

// postgresCollector polls one PostgreSQL database through a single session
// (see dbsession.go for the connection rules).
type postgresCollector struct {
	dsn          string
	dbName       string
	debug        bool
	debugDone    bool
	session      dbSession
	poller       poller[PostgresStats]
	prev         pgRaw
	prevAt       time.Time
	serverVerNum int // server_version_num (e.g. 170000 = PG17); 0 until connected
}

// pgValue quotes a libpq connection-string value, so no configured value can
// end its own field and add parameters of its own.
func pgValue(v string) string {
	escaped := strings.ReplaceAll(v, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `'`, `\'`)
	return "'" + escaped + "'"
}

// postgresDSN builds the connection string. Besides the connection target it
// carries the session settings every PostgreSQL 10+ server accepts at
// startup, so they apply from the first statement of every connection:
// statement, lock and idle-in-transaction timeouts end runaway work, and the
// server-side TCP keepalives end the session within about a minute if this
// host vanishes. application_name marks the session as Kula's in
// pg_stat_activity.
func postgresDSN(host string, port int, user, password, dbname, sslmode string, t dbTimeouts) string {
	parts := []string{"host=" + pgValue(host)}
	if port != 0 {
		parts = append(parts, fmt.Sprintf("port=%d", port))
	}
	parts = append(parts,
		"user="+pgValue(user),
		"dbname="+pgValue(dbname),
		"sslmode="+pgValue(sslmode),
	)
	if password != "" {
		parts = append(parts, "password="+pgValue(password))
	}
	parts = append(parts,
		"application_name='kula'",
		fmt.Sprintf("statement_timeout=%d", t.statement.Milliseconds()),
		fmt.Sprintf("lock_timeout=%d", t.lock.Milliseconds()),
		fmt.Sprintf("idle_in_transaction_session_timeout=%d", t.poll.Milliseconds()),
		"tcp_keepalives_idle=30",
		"tcp_keepalives_interval=10",
		"tcp_keepalives_count=3",
	)
	return strings.Join(parts, " ")
}

// pgDialer dials PostgreSQL connections and keeps track of the ones still
// open. Its keepalives probe an idle connection after 15 s and give up after
// three unanswered probes 5 s apart, so a server that vanished while the
// session sat idle is noticed in seconds instead of the default's minutes.
// lib/pq does not close a socket when a query's context ends (it sends a
// cancel request and keeps reading), and keepalives cannot help a socket with
// a read in flight to a frozen server, so abort closes every open connection:
// dbSession.bound calls it for work stuck past its deadline.
type pgDialer struct {
	d     net.Dialer
	mu    sync.Mutex
	conns map[*pgConn]struct{}
}

// pgConn removes itself from its dialer's open set when closed.
type pgConn struct {
	net.Conn
	owner *pgDialer
	once  sync.Once
}

func (c *pgConn) Close() error {
	c.once.Do(func() {
		c.owner.mu.Lock()
		delete(c.owner.conns, c)
		c.owner.mu.Unlock()
	})
	return c.Conn.Close()
}

func newPgDialer() *pgDialer {
	return &pgDialer{
		d: net.Dialer{KeepAliveConfig: net.KeepAliveConfig{
			Enable: true, Idle: 15 * time.Second, Interval: 5 * time.Second, Count: 3,
		}},
		conns: make(map[*pgConn]struct{}),
	}
}

func (d *pgDialer) track(conn net.Conn, err error) (net.Conn, error) {
	if err != nil {
		return nil, err
	}
	c := &pgConn{Conn: conn, owner: d}
	d.mu.Lock()
	d.conns[c] = struct{}{}
	d.mu.Unlock()
	return c, nil
}

func (d *pgDialer) Dial(network, address string) (net.Conn, error) {
	return d.track(d.d.Dial(network, address))
}

func (d *pgDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.track(d.d.DialContext(ctx, network, address))
}

func (d *pgDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.track(d.d.DialContext(ctx, network, address))
}

// abort closes every connection the dialer opened that is still open,
// failing any read or write blocked on one.
func (d *pgDialer) abort() {
	d.mu.Lock()
	open := make([]*pgConn, 0, len(d.conns))
	for c := range d.conns {
		open = append(open, c)
	}
	d.mu.Unlock()
	for _, c := range open {
		_ = c.Close()
	}
}

// newPostgresCollector prepares the collector without connecting; the first
// poll connects.
func newPostgresCollector(host string, port int, user, password, dbname, sslmode string, debug bool, interval time.Duration) *postgresCollector {
	t := newDBTimeouts(interval)
	pc := &postgresCollector{
		dsn:    postgresDSN(host, port, user, password, dbname, sslmode, t),
		dbName: dbname,
		debug:  debug,
	}
	pc.poller.stale = t.stale
	dialer := newPgDialer()
	pc.session = dbSession{
		name:     "[postgres]",
		label:    dbname,
		timeouts: t,
		dialect: dbDialect{
			setup:    pc.setupSession,
			reap:     reapPostgresSession,
			describe: func(ref dbSessionRef) string { return fmt.Sprintf("pid %d", ref.id) },
			abort:    dialer.abort,
		},
	}
	if connector, err := pq.NewConnector(pc.dsn); err == nil {
		connector.Dialer(dialer)
		pc.session.connector = connector
	} else {
		log.Printf("[postgres] invalid connection settings for %q: %v", dbname, err)
	}
	return pc
}

// setupSession records the session's identity and server version and adds
// the settings only newer servers know: idle_session_timeout (14+) ends a
// session nothing is sent on, client_connection_check_interval (14+) lets a
// long query notice that this client has gone, tcp_user_timeout (12+) ends a
// connection whose data goes unacknowledged.
func (pc *postgresCollector) setupSession(ctx context.Context, conn *sql.Conn) (dbSessionRef, error) {
	var ref dbSessionRef
	var version int
	if err := conn.QueryRowContext(ctx, `
		SELECT pid, backend_start::text, current_setting('server_version_num')::int
		FROM pg_stat_activity WHERE pid = pg_backend_pid()
	`).Scan(&ref.id, &ref.started, &version); err != nil {
		return ref, err
	}
	pc.serverVerNum = version
	var settings []string
	if version >= 120000 {
		settings = append(settings, "SET tcp_user_timeout = 30000")
	}
	if version >= 140000 {
		settings = append(settings,
			fmt.Sprintf("SET idle_session_timeout = %d", pc.session.timeouts.idle.Milliseconds()),
			"SET client_connection_check_interval = 5000")
	}
	for _, statement := range settings {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			pc.debugf("[postgres] %s: %v", statement, err)
		}
	}
	return ref, nil
}

// reapPostgresSession terminates an abandoned backend. pid and backend_start
// together identify one backend for the server's lifetime, so a pid reused by
// another client's session never matches. pg_terminate_backend works on the
// same role's backends without extra privileges.
func reapPostgresSession(ctx context.Context, conn *sql.Conn, ref dbSessionRef) (bool, error) {
	alive := func() (bool, error) {
		var exists bool
		err := conn.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1 AND backend_start = $2::timestamptz)
		`, ref.id, ref.started).Scan(&exists)
		return exists, err
	}
	exists, err := alive()
	if err != nil || !exists {
		return err == nil, err
	}
	var signalled sql.NullBool
	err = conn.QueryRowContext(ctx, `
		SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE pid = $1 AND backend_start = $2::timestamptz
	`, ref.id, ref.started).Scan(&signalled)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	// The backend exits once it handles the signal; give it a moment.
	for range 10 {
		if exists, err = alive(); err != nil || !exists {
			return err == nil, err
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return false, nil
}

func (pc *postgresCollector) debugf(format string, args ...any) {
	if pc.debug && !pc.debugDone {
		log.Printf(format, args...)
	}
}

// Start begins polling in the background.
func (pc *postgresCollector) Start(ctx context.Context) {
	pc.poller.start(ctx, pc.session.timeouts.interval, pc.poll, pc.session.release)
}

// Latest returns the newest metrics while they are current.
func (pc *postgresCollector) Latest() *PostgresStats {
	return pc.poller.Latest()
}

// Close waits for the poller, stopped by its context, to end its session.
func (pc *postgresCollector) Close() {
	pc.poller.wait(5 * time.Second)
}

// poll runs one collection on the collector's session.
func (pc *postgresCollector) poll(ctx context.Context) *PostgresStats {
	if pc.session.connector == nil {
		return nil
	}
	conn, err := pc.session.acquire(ctx)
	if err != nil {
		pc.prev = pgRaw{}
		return nil
	}
	pollCtx, cancel := pc.session.bound(ctx, pc.session.timeouts.poll)
	defer cancel()
	stats, err := pc.query(pollCtx, conn)
	if err == nil {
		// A non-fatal query cut short by the deadline still means the
		// session may be busy on the server.
		err = pollCtx.Err()
	}
	if err != nil {
		// Rates are deltas between consecutive successful polls.
		pc.prev = pgRaw{}
		pc.session.fail(ctx, err)
		return nil
	}
	pc.session.succeed()
	pc.debugDone = true
	return stats
}

// query gathers PostgreSQL metrics. Errors of the queries every poll needs
// fail it; the others are left out of the result.
func (pc *postgresCollector) query(ctx context.Context, conn *sql.Conn) (*PostgresStats, error) {
	stats := &PostgresStats{}

	// Connection states from pg_stat_activity:
	// active    — executing queries (excluding lock waiters)
	// idle      — open but doing nothing
	// idle_in_tx — started a transaction but not committed/rolled back (dangerous)
	// waiting   — blocked waiting for a lock
	pc.debugf("[postgres] querying pg_stat_activity for %q", pc.dbName)
	row := conn.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN state = 'active'
			                   AND (wait_event_type IS DISTINCT FROM 'Lock') THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN state = 'idle' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN state IN ('idle in transaction',
			                                 'idle in transaction (aborted)') THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN wait_event_type = 'Lock' THEN 1 ELSE 0 END), 0)
		FROM pg_stat_activity
		WHERE backend_type = 'client backend'
	`)
	if err := row.Scan(&stats.ActiveConns, &stats.IdleConns,
		&stats.IdleInTxConns, &stats.WaitingConns); err != nil {
		return nil, fmt.Errorf("pg_stat_activity: %w", err)
	}

	// Max connections setting
	var maxConnsStr string
	if err := conn.QueryRowContext(ctx, "SHOW max_connections").Scan(&maxConnsStr); err == nil {
		if v, err := strconv.Atoi(maxConnsStr); err == nil {
			stats.MaxConns = v
		}
	}

	// Database-level cumulative counters from pg_stat_database
	var cur pgRaw
	row = conn.QueryRowContext(ctx, `
		SELECT
			COALESCE(xact_commit,   0),
			COALESCE(xact_rollback, 0),
			COALESCE(tup_fetched,   0),
			COALESCE(tup_returned,  0),
			COALESCE(tup_inserted,  0),
			COALESCE(tup_updated,   0),
			COALESCE(tup_deleted,   0),
			COALESCE(blks_read,     0),
			COALESCE(blks_hit,      0),
			COALESCE(deadlocks,     0)
		FROM pg_stat_database
		WHERE datname = $1
	`, pc.dbName)
	if err := row.Scan(
		&cur.xactCommit, &cur.xactRollback,
		&cur.tupFetched, &cur.tupReturned,
		&cur.tupInserted, &cur.tupUpdated, &cur.tupDeleted,
		&cur.blksRead, &cur.blksHit,
		&cur.deadlocks,
	); err != nil {
		return nil, fmt.Errorf("pg_stat_database: %w", err)
	}
	cur.valid = true
	readAt := time.Now()

	pc.debugf("[postgres] raw: commit=%d, rollback=%d, hit=%d, read=%d, deadlocks=%d",
		cur.xactCommit, cur.xactRollback, cur.blksHit, cur.blksRead, cur.deadlocks)

	// Background writer cumulative counters.
	// PG17 split pg_stat_bgwriter: checkpoint stats moved to
	// pg_stat_checkpointer, and buffers_backend was removed entirely —
	// the closest equivalent is now SUM(writes) over pg_stat_io for
	// non-bgwriter/non-checkpointer backends.
	var bgQuery string
	if pc.serverVerNum >= 170000 {
		bgQuery = `
			SELECT
				COALESCE((SELECT buffers_written FROM pg_stat_checkpointer), 0),
				COALESCE((SELECT SUM(writes) FROM pg_stat_io
				           WHERE object = 'relation'
				             AND backend_type NOT IN ('background writer', 'checkpointer')), 0)
		`
	} else {
		bgQuery = `
			SELECT
				COALESCE(buffers_checkpoint, 0),
				COALESCE(buffers_backend,    0)
			FROM pg_stat_bgwriter
		`
	}
	row = conn.QueryRowContext(ctx, bgQuery)
	if err := row.Scan(&cur.bufCheckpoint, &cur.bufBackend); err != nil {
		pc.debugf("[postgres] bgwriter query error: %v", err)
		// Non-fatal: continue without bgwriter data. The zeroed pair must not
		// become a baseline, or the next good poll would report the lifetime
		// totals as one interval's writes.
		cur.bufCheckpoint, cur.bufBackend = 0, 0
	} else {
		cur.bgValid = true
	}

	elapsed := 0.0
	if !pc.prevAt.IsZero() {
		elapsed = readAt.Sub(pc.prevAt).Seconds()
	}
	pc.calculateStats(stats, cur, elapsed)
	pc.prevAt = readAt

	// Table health across all user tables: dead/live tuples and autovacuum activity
	row = conn.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(n_dead_tup),       0),
			COALESCE(SUM(n_live_tup),       0),
			COALESCE(SUM(autovacuum_count), 0)
		FROM pg_stat_user_tables
	`)
	var deadTup, liveTup, vacCount sql.NullInt64
	if err := row.Scan(&deadTup, &liveTup, &vacCount); err == nil {
		if deadTup.Valid {
			stats.DeadTuples = deadTup.Int64
		}
		if liveTup.Valid {
			stats.LiveTuples = liveTup.Int64
		}
		if vacCount.Valid {
			stats.AutovacuumCount = vacCount.Int64
		}
	}

	// Database size
	var dbSize sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		"SELECT pg_database_size($1)", pc.dbName,
	).Scan(&dbSize); err == nil && dbSize.Valid {
		stats.DBSizeBytes = dbSize.Int64
	}

	// Replication state. All queries here are non-fatal: a user without
	// pg_monitor/REPLICATION privileges should still get the rest of the
	// metrics. On primary, ReplicaCount is the count of attached standbys.
	// On standby, the two lag fields are derived from
	// pg_last_wal_receive_lsn/pg_last_wal_replay_lsn and
	// pg_last_xact_replay_timestamp.
	var inRecovery bool
	if err := conn.QueryRowContext(ctx,
		"SELECT pg_is_in_recovery()",
	).Scan(&inRecovery); err == nil {
		stats.IsInRecovery = inRecovery
	} else {
		pc.debugf("[postgres] pg_is_in_recovery error: %v", err)
	}

	if !stats.IsInRecovery {
		var rcount sql.NullInt64
		if err := conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM pg_stat_replication",
		).Scan(&rcount); err == nil && rcount.Valid {
			stats.ReplicaCount = int(rcount.Int64)
		} else if err != nil {
			pc.debugf("[postgres] pg_stat_replication query error: %v", err)
		}
	} else {
		var lagBytes sql.NullInt64
		var lagSecs sql.NullFloat64
		// Cast pg_wal_lsn_diff's numeric result to bigint so lib/pq can scan
		// it directly into NullInt64. GREATEST(0, ...) on the time delta
		// guards against brief negative values caused by clock skew between
		// primary and standby (NTP drift can leave the standby clock a few
		// hundred ms ahead of the primary's last commit timestamp).
		if err := conn.QueryRowContext(ctx, `
			SELECT
				COALESCE(pg_wal_lsn_diff(pg_last_wal_receive_lsn(),
				                         pg_last_wal_replay_lsn())::bigint, 0),
				GREATEST(0, COALESCE(EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp())), 0))
		`).Scan(&lagBytes, &lagSecs); err == nil {
			if lagBytes.Valid {
				stats.ReplicationLagBytes = lagBytes.Int64
			}
			if lagSecs.Valid {
				stats.ReplicationLagSeconds = round2(lagSecs.Float64)
			}
		} else {
			pc.debugf("[postgres] replication lag query error: %v", err)
		}
	}

	return stats, nil
}

// calculateStats computes per-second rates from raw cumulative counters. Each
// counter group needs a valid reading at both ends of the interval.
func (pc *postgresCollector) calculateStats(stats *PostgresStats, cur pgRaw, elapsed float64) {
	rate := func(cur, prev int64) float64 {
		if cur < prev {
			return 0 // counter reset on server restart
		}
		return round2(float64(cur-prev) / elapsed)
	}
	if pc.prev.valid && cur.valid && elapsed > 0 {
		stats.TxCommitPS = rate(cur.xactCommit, pc.prev.xactCommit)
		stats.TxRollbackPS = rate(cur.xactRollback, pc.prev.xactRollback)
		stats.TupFetchedPS = rate(cur.tupFetched, pc.prev.tupFetched)
		stats.TupReturnedPS = rate(cur.tupReturned, pc.prev.tupReturned)
		stats.TupInsertedPS = rate(cur.tupInserted, pc.prev.tupInserted)
		stats.TupUpdatedPS = rate(cur.tupUpdated, pc.prev.tupUpdated)
		stats.TupDeletedPS = rate(cur.tupDeleted, pc.prev.tupDeleted)
		stats.BlksReadPS = rate(cur.blksRead, pc.prev.blksRead)
		stats.BlksHitPS = rate(cur.blksHit, pc.prev.blksHit)
		stats.DeadlocksPS = rate(cur.deadlocks, pc.prev.deadlocks)
	}
	if pc.prev.bgValid && cur.bgValid && elapsed > 0 {
		stats.BufCheckpointPS = rate(cur.bufCheckpoint, pc.prev.bufCheckpoint)
		stats.BufBackendPS = rate(cur.bufBackend, pc.prev.bufBackend)
	}
	pc.prev = cur

	// Cache hit ratio from cumulative totals (more stable than deriving from rates)
	total := cur.blksRead + cur.blksHit
	if total > 0 {
		stats.BlksHitPct = round2(float64(cur.blksHit) / float64(total) * 100)
	}
}
