package collector

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	// PostgreSQL driver — imported for side-effect registration.
	_ "github.com/lib/pq"
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

// postgresCollector manages the PostgreSQL connection and metrics.
type postgresCollector struct {
	dsn          string
	db           *sql.DB
	dbName       string
	prev         pgRaw
	debug        bool
	wasConnected bool          // tracks connection state for log transitions
	timeout      time.Duration // per-query/connect timeout, derived from collection interval
	serverVerNum int           // server_version_num (e.g. 170000 = PG17); 0 until detected
}

// newPostgresCollector builds the DSN and returns a collector (without connecting yet).
// Connection is lazy — established on first Collect() call.
func newPostgresCollector(host string, port int, user, password, dbname, sslmode string, debug bool, timeout time.Duration) *postgresCollector {
	var dsn string
	if port == 0 {
		// Unix socket mode: host is the socket directory
		dsn = fmt.Sprintf("host=%s user=%s dbname=%s sslmode=%s",
			host, user, dbname, sslmode)
	} else {
		dsn = fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=%s",
			host, port, user, dbname, sslmode)
	}
	if password != "" {
		// libpq key=value format requires single-quoting values that contain
		// spaces or special characters; backslashes and single quotes within
		// the quoted value must themselves be escaped with a backslash.
		escaped := strings.ReplaceAll(password, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `'`, `\'`)
		dsn += " password='" + escaped + "'"
	}

	return &postgresCollector{
		dsn:     dsn,
		dbName:  dbname,
		debug:   debug,
		timeout: timeout,
	}
}

// connect establishes (or verifies) the DB connection.
func (pc *postgresCollector) connect() error {
	if pc.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), pc.timeout)
		defer cancel()
		if err := pc.db.PingContext(ctx); err == nil {
			return nil // already connected
		}
		// Connection lost, close and retry.
		// Clear prev counters so we don't compute bogus rates against
		// stale values from a previous postgres instance.
		_ = pc.db.Close()
		pc.db = nil
		pc.prev = pgRaw{}
		pc.serverVerNum = 0
		if pc.wasConnected {
			log.Printf("[postgres] connection to %q lost, will retry", pc.dbName)
			pc.wasConnected = false
		}
	}

	db, err := sql.Open("postgres", pc.dsn)
	if err != nil {
		return fmt.Errorf("postgres open: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), pc.timeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return fmt.Errorf("postgres ping: %w", err)
	}
	pc.db = db

	var verStr string
	if err := db.QueryRowContext(ctx, "SHOW server_version_num").Scan(&verStr); err == nil {
		if v, err := strconv.Atoi(verStr); err == nil {
			pc.serverVerNum = v
		}
	}

	if !pc.wasConnected {
		log.Printf("[postgres] connected to database %q", pc.dbName)
		pc.wasConnected = true
	}
	return nil
}

// Close closes the database connection.
func (pc *postgresCollector) Close() {
	if pc.db != nil {
		_ = pc.db.Close()
	}
}

// collectPostgres gathers PostgreSQL metrics. Returns nil on any error.
func (c *Collector) collectPostgres(elapsed float64) *PostgresStats {
	if c.pgCollector == nil {
		return nil
	}

	if err := c.pgCollector.connect(); err != nil {
		// Logged via state transitions in connect(); suppress per-cycle noise.
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.collCfg.Interval)
	defer cancel()

	// Rates are deltas between consecutive successful polls. A poll that
	// fails before its counters are recorded drops the baseline, so the next
	// one starts over instead of dividing two intervals' growth by one.
	recorded := false
	defer func() {
		if !recorded {
			c.pgCollector.prev = pgRaw{}
		}
	}()

	stats := &PostgresStats{}

	// Connection states from pg_stat_activity:
	// active    — executing queries (excluding lock waiters)
	// idle      — open but doing nothing
	// idle_in_tx — started a transaction but not committed/rolled back (dangerous)
	// waiting   — blocked waiting for a lock
	c.debugf("[postgres] querying pg_stat_activity for %q", c.pgCollector.dbName)
	row := c.pgCollector.db.QueryRowContext(ctx, `
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
		c.debugf("[postgres] scan activity error: %v", err)
		return nil
	}

	// Max connections setting
	var maxConnsStr string
	if err := c.pgCollector.db.QueryRowContext(ctx, "SHOW max_connections").Scan(&maxConnsStr); err == nil {
		if v, err := strconv.Atoi(maxConnsStr); err == nil {
			stats.MaxConns = v
		}
	}

	// Database-level cumulative counters from pg_stat_database
	var cur pgRaw
	row = c.pgCollector.db.QueryRowContext(ctx, `
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
	`, c.pgCollector.dbName)
	if err := row.Scan(
		&cur.xactCommit, &cur.xactRollback,
		&cur.tupFetched, &cur.tupReturned,
		&cur.tupInserted, &cur.tupUpdated, &cur.tupDeleted,
		&cur.blksRead, &cur.blksHit,
		&cur.deadlocks,
	); err != nil {
		c.appErrorf("[postgres] pg_stat_database query error: %v", err)
		return nil
	}
	cur.valid = true

	c.debugf("[postgres] raw: commit=%d, rollback=%d, hit=%d, read=%d, deadlocks=%d",
		cur.xactCommit, cur.xactRollback, cur.blksHit, cur.blksRead, cur.deadlocks)

	// Background writer cumulative counters.
	// PG17 split pg_stat_bgwriter: checkpoint stats moved to
	// pg_stat_checkpointer, and buffers_backend was removed entirely —
	// the closest equivalent is now SUM(writes) over pg_stat_io for
	// non-bgwriter/non-checkpointer backends.
	var bgQuery string
	if c.pgCollector.serverVerNum >= 170000 {
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
	row = c.pgCollector.db.QueryRowContext(ctx, bgQuery)
	if err := row.Scan(&cur.bufCheckpoint, &cur.bufBackend); err != nil {
		c.debugf("[postgres] bgwriter query error: %v", err)
		// Non-fatal: continue without bgwriter data. The zeroed pair must not
		// become a baseline, or the next good poll would report the lifetime
		// totals as one interval's writes.
		cur.bufCheckpoint, cur.bufBackend = 0, 0
	} else {
		cur.bgValid = true
	}

	c.pgCollector.calculateStats(stats, cur, elapsed)
	recorded = true

	// Table health across all user tables: dead/live tuples and autovacuum activity
	row = c.pgCollector.db.QueryRowContext(ctx, `
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
	if err := c.pgCollector.db.QueryRowContext(ctx,
		"SELECT pg_database_size($1)", c.pgCollector.dbName,
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
	if err := c.pgCollector.db.QueryRowContext(ctx,
		"SELECT pg_is_in_recovery()",
	).Scan(&inRecovery); err == nil {
		stats.IsInRecovery = inRecovery
	} else {
		c.debugf("[postgres] pg_is_in_recovery error: %v", err)
	}

	if !stats.IsInRecovery {
		var rcount sql.NullInt64
		if err := c.pgCollector.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM pg_stat_replication",
		).Scan(&rcount); err == nil && rcount.Valid {
			stats.ReplicaCount = int(rcount.Int64)
		} else if err != nil {
			c.debugf("[postgres] pg_stat_replication query error: %v", err)
		}
	} else {
		var lagBytes sql.NullInt64
		var lagSecs sql.NullFloat64
		// Cast pg_wal_lsn_diff's numeric result to bigint so lib/pq can scan
		// it directly into NullInt64. GREATEST(0, ...) on the time delta
		// guards against brief negative values caused by clock skew between
		// primary and standby (NTP drift can leave the standby clock a few
		// hundred ms ahead of the primary's last commit timestamp).
		if err := c.pgCollector.db.QueryRowContext(ctx, `
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
			c.debugf("[postgres] replication lag query error: %v", err)
		}
	}

	return stats
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
