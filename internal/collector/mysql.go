package collector

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"time"

	_ "github.com/go-sql-driver/mysql"
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

type mysqlCollector struct {
	dsn          string
	db           *sql.DB
	label        string
	prev         mysqlRaw
	debug        bool
	wasConnected bool
	lastErr      string
	timeout      time.Duration
}

func newMysqlCollector(host string, port int, user, password, dbname string, debug bool, timeout time.Duration) *mysqlCollector {
	var dsn string
	if port == 0 {
		dsn = fmt.Sprintf("%s:%s@unix(%s)/%s", user, password, host, dbname)
	} else {
		dsn = fmt.Sprintf("%s:%s@tcp(%s:%d)/%s", user, password, host, port, dbname)
	}
	dsn += "?timeout=5s&readTimeout=5s"

	label := dbname
	if label == "" {
		if port == 0 {
			label = fmt.Sprintf("socket %s", host)
		} else {
			label = fmt.Sprintf("%s:%d", host, port)
		}
	}

	return &mysqlCollector{
		dsn:     dsn,
		label:   label,
		debug:   debug,
		timeout: timeout,
	}
}

func (mc *mysqlCollector) connect() error {
	if mc.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), mc.timeout)
		defer cancel()
		if err := mc.db.PingContext(ctx); err == nil {
			return nil
		}
		_ = mc.db.Close()
		mc.db = nil
		mc.prev = mysqlRaw{}
		mc.lastErr = ""
		if mc.wasConnected {
			log.Printf("[mysql] connection to %q lost, will retry", mc.label)
			mc.wasConnected = false
		}
	}

	db, err := sql.Open("mysql", mc.dsn)
	if err != nil {
		errStr := err.Error()
		if errStr != mc.lastErr {
			log.Printf("[mysql] failed to open database %q: %v", mc.label, err)
			mc.lastErr = errStr
		}
		return fmt.Errorf("mysql open: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), mc.timeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		errStr := err.Error()
		if errStr != mc.lastErr {
			log.Printf("[mysql] failed to ping database %q: %v", mc.label, err)
			mc.lastErr = errStr
		}
		return fmt.Errorf("mysql ping: %w", err)
	}
	mc.db = db
	mc.lastErr = ""
	if !mc.wasConnected {
		log.Printf("[mysql] connected to %q", mc.label)
		mc.wasConnected = true
	}
	return nil
}

func (mc *mysqlCollector) Close() {
	if mc.db != nil {
		_ = mc.db.Close()
	}
}

func (c *Collector) collectMysql(elapsed float64) *MysqlStats {
	if c.myCollector == nil {
		return nil
	}

	if err := c.myCollector.connect(); err != nil {
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
			c.myCollector.prev = mysqlRaw{}
		}
	}()

	stats := &MysqlStats{}

	rows, err := c.myCollector.db.QueryContext(ctx, "SHOW GLOBAL STATUS")
	if err != nil {
		c.debugf("[mysql] SHOW GLOBAL STATUS error: %v", err)
		return nil
	}
	defer func() { _ = rows.Close() }()

	statusVars := make(map[string]string)
	for rows.Next() {
		var name, val string
		if err := rows.Scan(&name, &val); err != nil {
			return nil
		}
		statusVars[name] = val
	}

	if err := rows.Err(); err != nil {
		c.debugf("[mysql] SHOW GLOBAL STATUS iteration error: %v", err)
		return nil
	}

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

	c.debugf("[mysql] raw: questions=%d, select=%d, insert=%d, update=%d, delete=%d, slow=%d, bp_reads=%d, bp_reqs=%d",
		cur.questions, cur.comSelect, cur.comInsert, cur.comUpdate, cur.comDelete,
		cur.slowQueries, cur.innodbBPReads, cur.innodbBPRequests)

	var maxConnsStr string
	if err := c.myCollector.db.QueryRowContext(ctx, "SELECT @@max_connections").Scan(&maxConnsStr); err == nil {
		if v, err := strconv.Atoi(maxConnsStr); err == nil {
			stats.MaxConnections = v
		}
	}

	c.myCollector.calculateStats(stats, cur, elapsed)
	recorded = true

	// Replication: sentinel -1 means "not configured / NULL". Queries here
	// are non-fatal — a server without replication or a user without
	// REPLICATION CLIENT privilege should still get all the other metrics.
	stats.ReplicaSecondsBehind = -1
	c.collectMysqlReplicaStatus(ctx, stats)
	c.collectMysqlReplicaCount(ctx, stats)

	return stats
}

// collectMysqlReplicaStatus fills ReplicaIORunning, ReplicaSQLRunning, and
// ReplicaSecondsBehind from SHOW REPLICA STATUS, falling back to the legacy
// SHOW SLAVE STATUS syntax for older MySQL/MariaDB.
func (c *Collector) collectMysqlReplicaStatus(ctx context.Context, stats *MysqlStats) {
	rows, err := c.myCollector.db.QueryContext(ctx, "SHOW REPLICA STATUS")
	if err != nil {
		rows, err = c.myCollector.db.QueryContext(ctx, "SHOW SLAVE STATUS")
		if err != nil {
			c.debugf("[mysql] SHOW REPLICA/SLAVE STATUS error: %v", err)
			return
		}
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		c.debugf("[mysql] replica status columns error: %v", err)
		return
	}
	if !rows.Next() {
		// rows.Next() returns false on iteration error too, not just EOF.
		// Surface the error in debug logs so a transient network failure
		// isn't silently misread as "not configured as a replica".
		if err := rows.Err(); err != nil {
			c.debugf("[mysql] replica status iter error: %v", err)
		}
		return
	}

	vals := make([]sql.NullString, len(cols))
	scanArgs := make([]interface{}, len(cols))
	for i := range vals {
		scanArgs[i] = &vals[i]
	}
	if err := rows.Scan(scanArgs...); err != nil {
		c.debugf("[mysql] replica status scan error: %v", err)
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
func (c *Collector) collectMysqlReplicaCount(ctx context.Context, stats *MysqlStats) {
	rows, err := c.myCollector.db.QueryContext(ctx, "SHOW REPLICAS")
	if err != nil {
		rows, err = c.myCollector.db.QueryContext(ctx, "SHOW SLAVE HOSTS")
		if err != nil {
			c.debugf("[mysql] SHOW REPLICAS/SLAVE HOSTS error: %v", err)
			return
		}
	}
	defer func() { _ = rows.Close() }()

	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		c.debugf("[mysql] replica count iteration error: %v", err)
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
