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

	c.debugf("[postgres] raw: commit=%d, rollback=%d, hit=%d, read=%d, deadlocks=%d",
		cur.xactCommit, cur.xactRollback, cur.blksHit, cur.blksRead, cur.deadlocks)

	// Background writer cumulative counters from pg_stat_bgwriter
	row = c.pgCollector.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(buffers_checkpoint, 0),
			COALESCE(buffers_backend,    0)
		FROM pg_stat_bgwriter
	`)
	if err := row.Scan(&cur.bufCheckpoint, &cur.bufBackend); err != nil {
		c.debugf("[postgres] pg_stat_bgwriter query error: %v", err)
		// non-fatal: continue without bgwriter data
	}

	c.pgCollector.calculateStats(stats, cur, elapsed)

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

	return stats
}

// calculateStats computes per-second rates from raw cumulative counters.
func (pc *postgresCollector) calculateStats(stats *PostgresStats, cur pgRaw, elapsed float64) {
	if pc.prev.xactCommit > 0 && elapsed > 0 {
		rate := func(cur, prev int64) float64 {
			if cur < prev {
				return 0 // counter reset on server restart
			}
			return round2(float64(cur-prev) / elapsed)
		}
		stats.TxCommitPS      = rate(cur.xactCommit,    pc.prev.xactCommit)
		stats.TxRollbackPS    = rate(cur.xactRollback,  pc.prev.xactRollback)
		stats.TupFetchedPS    = rate(cur.tupFetched,    pc.prev.tupFetched)
		stats.TupReturnedPS   = rate(cur.tupReturned,   pc.prev.tupReturned)
		stats.TupInsertedPS   = rate(cur.tupInserted,   pc.prev.tupInserted)
		stats.TupUpdatedPS    = rate(cur.tupUpdated,    pc.prev.tupUpdated)
		stats.TupDeletedPS    = rate(cur.tupDeleted,    pc.prev.tupDeleted)
		stats.BlksReadPS      = rate(cur.blksRead,      pc.prev.blksRead)
		stats.BlksHitPS       = rate(cur.blksHit,       pc.prev.blksHit)
		stats.DeadlocksPS     = rate(cur.deadlocks,     pc.prev.deadlocks)
		stats.BufCheckpointPS = rate(cur.bufCheckpoint, pc.prev.bufCheckpoint)
		stats.BufBackendPS    = rate(cur.bufBackend,    pc.prev.bufBackend)
	}
	pc.prev = cur

	// Cache hit ratio from cumulative totals (more stable than deriving from rates)
	total := cur.blksRead + cur.blksHit
	if total > 0 {
		stats.BlksHitPct = round2(float64(cur.blksHit) / float64(total) * 100)
	}
}
