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
	"time"

	"github.com/go-sql-driver/mysql"
)

type mysqlRaw struct {
	questions        int64
	comSelect        int64
	comInsert        int64
	comUpdate        int64
	comDelete        int64
	slowQueries      int64
	innodbBPReads    int64
	innodbBPRequests int64
	tableLocksWaited int64
	rowLockWaits     int64
	// valid marks counters read from the server; a rate needs both ends.
	valid bool
}

// mysqlCollector polls one MySQL or MariaDB server through a single session
// (see dbsession.go for the connection rules).
type mysqlCollector struct {
	label     string
	debug     bool
	debugDone bool
	config    *mysql.Config
	session   dbSession
	poller    poller[MysqlStats]
	prev      mysqlRaw
	prevAt    time.Time
}

// mysqlConfig builds the driver configuration. Every I/O operation is bounded
// by the poll timeout, and two session settings that MySQL and MariaDB both
// know are applied on connect: wait_timeout ends the session once nothing has
// been sent on it for a while (a vanished client), lock_wait_timeout keeps a
// statement from queueing behind a metadata lock. The program_name connection
// attribute marks the session as Kula's.
func mysqlConfig(host string, port int, user, password, dbname string, t dbTimeouts) *mysql.Config {
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.DBName = user, password, dbname
	if port == 0 {
		cfg.Net, cfg.Addr = "unix", host
	} else {
		cfg.Net, cfg.Addr = "tcp", net.JoinHostPort(host, strconv.Itoa(port))
	}
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = t.poll, t.poll, t.poll
	cfg.ConnectionAttributes = "program_name:kula"
	cfg.Params = map[string]string{
		"wait_timeout":      strconv.Itoa(int(t.idle.Seconds())),
		"lock_wait_timeout": strconv.Itoa(max(1, int(t.lock.Seconds()))),
	}
	return cfg
}

func newMysqlCollector(host string, port int, user, password, dbname string, debug bool, interval time.Duration) *mysqlCollector {
	t := newDBTimeouts(interval)
	label := dbname
	if label == "" {
		if port == 0 {
			label = fmt.Sprintf("socket %s", host)
		} else {
			label = net.JoinHostPort(host, strconv.Itoa(port))
		}
	}
	mc := &mysqlCollector{
		label:  label,
		debug:  debug,
		config: mysqlConfig(host, port, user, password, dbname, t),
	}
	mc.poller.stale = t.stale
	mc.poller.name, mc.poller.discard = "[mysql]", mc.discard
	dialer := newDBDialer()
	mc.config.DialFunc = dialer.DialContext
	mc.session = dbSession{
		name:     "[mysql]",
		label:    label,
		timeouts: t,
		dialect: dbDialect{
			setup:    mc.setupSession,
			reap:     reapMysqlSession,
			describe: func(ref dbSessionRef) string { return fmt.Sprintf("connection %d", ref.id) },
			abort:    dialer.abort,
			shut:     dialer.shut,
			reopen:   dialer.reopen,
		},
	}
	if connector, err := mysql.NewConnector(mc.config); err == nil {
		mc.session.connector = connector
	} else {
		log.Printf("[mysql] invalid connection settings for %q: %v", label, err)
	}
	return mc
}

// mysqlServerStart returns when the server started, in Unix seconds by its
// own clock. Connection ids are unique only within one server run.
func mysqlServerStart(ctx context.Context, conn *sql.Conn) (int64, error) {
	var name string
	var uptime, now int64
	if err := conn.QueryRowContext(ctx, "SHOW GLOBAL STATUS LIKE 'Uptime'").Scan(&name, &uptime); err != nil {
		return 0, err
	}
	if err := conn.QueryRowContext(ctx, "SELECT UNIX_TIMESTAMP()").Scan(&now); err != nil {
		return 0, err
	}
	return now - uptime, nil
}

// setupSession records the session's identity and bounds its statements on
// the server: MariaDB's max_statement_time covers every statement, MySQL's
// max_execution_time (5.7.8+) SELECT statements. An older server without the
// variable keeps its default.
func (mc *mysqlCollector) setupSession(ctx context.Context, conn *sql.Conn) (dbSessionRef, error) {
	var ref dbSessionRef
	var version string
	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID(), VERSION()").Scan(&ref.id, &version); err != nil {
		return ref, err
	}
	if err := conn.QueryRowContext(ctx,
		"SELECT USER FROM information_schema.PROCESSLIST WHERE ID = CONNECTION_ID()").Scan(&ref.user); err != nil {
		return ref, err
	}
	started, err := mysqlServerStart(ctx, conn)
	if err != nil {
		return ref, err
	}
	ref.started = strconv.FormatInt(started, 10)

	statement := fmt.Sprintf("SET SESSION max_execution_time = %d", mc.session.timeouts.statement.Milliseconds())
	if strings.Contains(strings.ToLower(version), "mariadb") {
		statement = fmt.Sprintf("SET SESSION max_statement_time = %g", mc.session.timeouts.statement.Seconds())
	}
	if _, err := conn.ExecContext(ctx, statement); err != nil {
		mc.debugf("[mysql] %s: %v", statement, err)
	}
	return ref, nil
}

// reapMysqlSession kills an abandoned connection. It acts only on a
// connection of the same user in the same server run, so an id the server
// has since given to another client is never touched; killing one's own
// connections needs no privilege.
func reapMysqlSession(ctx context.Context, conn *sql.Conn, ref dbSessionRef) (bool, error) {
	started, err := mysqlServerStart(ctx, conn)
	if err != nil {
		return false, err
	}
	if recorded, _ := strconv.ParseInt(ref.started, 10, 64); started-recorded > 5 || recorded-started > 5 {
		return true, nil // the server restarted: the session is gone with it
	}
	alive := func() (bool, error) {
		var n int
		err := conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE ID = ? AND USER = ?", ref.id, ref.user).Scan(&n)
		return n > 0, err
	}
	exists, err := alive()
	if err != nil || !exists {
		return err == nil, err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("KILL CONNECTION %d", ref.id)); err != nil {
		var serverErr *mysql.MySQLError
		if !errors.As(err, &serverErr) || serverErr.Number != 1094 { // 1094: unknown thread, already gone
			return false, err
		}
	}
	// A killed connection ends once its thread notices; give it a moment.
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

func (mc *mysqlCollector) debugf(format string, args ...any) {
	if mc.debug && !mc.debugDone {
		log.Printf(format, args...)
	}
}

// Start begins polling in the background.
func (mc *mysqlCollector) Start(ctx context.Context) {
	mc.poller.start(ctx, mc.session.timeouts.interval, mc.poll, mc.session.release)
}

// Latest returns the newest metrics while they are current.
func (mc *mysqlCollector) Latest() *MysqlStats {
	return mc.poller.Latest()
}

// Close waits for the poller, stopped by its context, to end its session.
func (mc *mysqlCollector) Close() {
	mc.poller.wait(5 * time.Second)
}

// discard drops the session and rate baseline of a poll that panicked.
func (mc *mysqlCollector) discard() {
	mc.prev = mysqlRaw{}
	mc.session.discard()
}

// poll runs one collection on the collector's session.
func (mc *mysqlCollector) poll(ctx context.Context) *MysqlStats {
	if mc.session.connector == nil {
		return nil
	}
	conn, err := mc.session.acquire(ctx)
	if err != nil {
		mc.prev = mysqlRaw{}
		return nil
	}
	pollCtx, cancel := mc.session.bound(ctx, mc.session.timeouts.poll)
	defer cancel()
	stats, err := mc.query(pollCtx, conn)
	if err == nil {
		err = pollCtx.Err()
	}
	if err != nil {
		// Rates are deltas between consecutive successful polls.
		mc.prev = mysqlRaw{}
		mc.session.fail(ctx, err)
		return nil
	}
	mc.session.succeed()
	mc.debugDone = true
	return stats
}

// mysqlGlobalStatus reads SHOW GLOBAL STATUS. Its rows are closed on return,
// before the session's next statement.
func mysqlGlobalStatus(ctx context.Context, conn *sql.Conn) (map[string]string, error) {
	rows, err := conn.QueryContext(ctx, "SHOW GLOBAL STATUS")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	vars := make(map[string]string)
	for rows.Next() {
		var name, val string
		if err := rows.Scan(&name, &val); err != nil {
			return nil, err
		}
		vars[name] = val
	}
	return vars, rows.Err()
}

// query gathers MySQL metrics. Errors of SHOW GLOBAL STATUS fail the poll;
// the other queries are left out of the result.
func (mc *mysqlCollector) query(ctx context.Context, conn *sql.Conn) (*MysqlStats, error) {
	stats := &MysqlStats{}

	statusVars, err := mysqlGlobalStatus(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("SHOW GLOBAL STATUS: %w", err)
	}
	readAt := time.Now()

	parseI := func(key string) int {
		v, _ := strconv.Atoi(statusVars[key])
		return v
	}
	parseI64 := func(key string) int64 {
		v, _ := strconv.ParseInt(statusVars[key], 10, 64)
		return v
	}

	stats.ThreadsConnected = parseI("Threads_connected")
	stats.ThreadsRunning = parseI("Threads_running")
	stats.ThreadsCached = parseI("Threads_cached")

	cur := mysqlRaw{valid: true}
	cur.questions = parseI64("Questions")
	cur.comSelect = parseI64("Com_select")
	cur.comInsert = parseI64("Com_insert")
	cur.comUpdate = parseI64("Com_update")
	cur.comDelete = parseI64("Com_delete")
	cur.slowQueries = parseI64("Slow_queries")
	cur.innodbBPReads = parseI64("Innodb_buffer_pool_reads")
	cur.innodbBPRequests = parseI64("Innodb_buffer_pool_read_requests")
	cur.tableLocksWaited = parseI64("Table_locks_waited")
	cur.rowLockWaits = parseI64("Innodb_row_lock_waits")

	mc.debugf("[mysql] raw: questions=%d, select=%d, insert=%d, update=%d, delete=%d, slow=%d, bp_reads=%d, bp_reqs=%d",
		cur.questions, cur.comSelect, cur.comInsert, cur.comUpdate, cur.comDelete,
		cur.slowQueries, cur.innodbBPReads, cur.innodbBPRequests)

	var maxConnsStr string
	if err := conn.QueryRowContext(ctx, "SELECT @@max_connections").Scan(&maxConnsStr); err == nil {
		if v, err := strconv.Atoi(maxConnsStr); err == nil {
			stats.MaxConnections = v
		}
	}

	elapsed := 0.0
	if !mc.prevAt.IsZero() {
		elapsed = readAt.Sub(mc.prevAt).Seconds()
	}
	mc.calculateStats(stats, cur, elapsed)
	mc.prevAt = readAt

	// Replication: sentinel -1 means "not configured / NULL". Queries here
	// are non-fatal — a server without replication or a user without
	// REPLICATION CLIENT privilege should still get all the other metrics.
	stats.ReplicaSecondsBehind = -1
	mc.replicaStatus(ctx, conn, stats)
	mc.replicaCount(ctx, conn, stats)

	return stats, nil
}

// collectMysqlReplicaStatus fills ReplicaIORunning, ReplicaSQLRunning, and
// ReplicaSecondsBehind from SHOW REPLICA STATUS, falling back to the legacy
// SHOW SLAVE STATUS syntax for older MySQL/MariaDB.
func (mc *mysqlCollector) replicaStatus(ctx context.Context, conn *sql.Conn, stats *MysqlStats) {
	rows, err := conn.QueryContext(ctx, "SHOW REPLICA STATUS")
	if err != nil {
		rows, err = conn.QueryContext(ctx, "SHOW SLAVE STATUS")
		if err != nil {
			mc.debugf("[mysql] SHOW REPLICA/SLAVE STATUS error: %v", err)
			return
		}
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		mc.debugf("[mysql] replica status columns error: %v", err)
		return
	}
	if !rows.Next() {
		// rows.Next() returns false on iteration error too, not just EOF.
		// Surface the error in debug logs so a transient network failure
		// isn't silently misread as "not configured as a replica".
		if err := rows.Err(); err != nil {
			mc.debugf("[mysql] replica status iter error: %v", err)
		}
		return
	}

	vals := make([]sql.NullString, len(cols))
	scanArgs := make([]interface{}, len(cols))
	for i := range vals {
		scanArgs[i] = &vals[i]
	}
	if err := rows.Scan(scanArgs...); err != nil {
		mc.debugf("[mysql] replica status scan error: %v", err)
		return
	}

	getStr := func(keys ...string) (string, bool) {
		for i, name := range cols {
			for _, k := range keys {
				if name == k {
					return vals[i].String, vals[i].Valid
				}
			}
		}
		return "", false
	}
	if v, ok := getStr("Replica_IO_Running", "Slave_IO_Running"); ok {
		stats.ReplicaIORunning = v == "Yes"
	}
	if v, ok := getStr("Replica_SQL_Running", "Slave_SQL_Running"); ok {
		stats.ReplicaSQLRunning = v == "Yes"
	}
	if v, ok := getStr("Seconds_Behind_Source", "Seconds_Behind_Master"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			stats.ReplicaSecondsBehind = n
		}
	}
	// Last_*_Errno columns are 0 when there is no error. They survive
	// thread restarts so an operator can see what the last failure was
	// even after manually restarting replication.
	if v, ok := getStr("Last_IO_Errno"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			stats.LastIOErrno = n
		}
	}
	if v, ok := getStr("Last_SQL_Errno"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			stats.LastSQLErrno = n
		}
	}
	// Slave_IO_State is the human-readable IO thread state. Cap at 200
	// bytes so a hostile / unusually long value can't blow the codec's
	// uint8-prefixed string limit (255 bytes).
	if v, ok := getStr("Replica_IO_State", "Slave_IO_State"); ok {
		if len(v) > 200 {
			v = v[:200]
		}
		stats.IOState = v
	}
}

// collectMysqlReplicaCount fills ReplicaCount from SHOW REPLICAS, falling back
// to SHOW SLAVE HOSTS for older servers.
func (mc *mysqlCollector) replicaCount(ctx context.Context, conn *sql.Conn, stats *MysqlStats) {
	rows, err := conn.QueryContext(ctx, "SHOW REPLICAS")
	if err != nil {
		rows, err = conn.QueryContext(ctx, "SHOW SLAVE HOSTS")
		if err != nil {
			mc.debugf("[mysql] SHOW REPLICAS/SLAVE HOSTS error: %v", err)
			return
		}
	}
	defer func() { _ = rows.Close() }()

	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		mc.debugf("[mysql] replica count iteration error: %v", err)
		return
	}
	stats.ReplicaCount = count
}

func (mc *mysqlCollector) calculateStats(stats *MysqlStats, cur mysqlRaw, elapsed float64) {
	if mc.prev.valid && cur.valid && elapsed > 0 {
		rate := func(cur, prev int64) float64 {
			if cur < prev {
				return 0
			}
			return round2(float64(cur-prev) / elapsed)
		}
		stats.QueriesPS = rate(cur.questions, mc.prev.questions)
		stats.ComSelectPS = rate(cur.comSelect, mc.prev.comSelect)
		stats.ComInsertPS = rate(cur.comInsert, mc.prev.comInsert)
		stats.ComUpdatePS = rate(cur.comUpdate, mc.prev.comUpdate)
		stats.ComDeletePS = rate(cur.comDelete, mc.prev.comDelete)
		stats.SlowQueriesPS = rate(cur.slowQueries, mc.prev.slowQueries)
		stats.InnodbBPReadsPS = rate(cur.innodbBPReads, mc.prev.innodbBPReads)
		stats.TableLocksWaitedPS = rate(cur.tableLocksWaited, mc.prev.tableLocksWaited)
		stats.RowLockWaitsPS = rate(cur.rowLockWaits, mc.prev.rowLockWaits)
	}
	mc.prev = cur

	if cur.innodbBPRequests > 0 {
		hits := cur.innodbBPRequests - cur.innodbBPReads
		stats.InnodbBufferPoolHitPct = round2(float64(hits) / float64(cur.innodbBPRequests) * 100)
	}
}
