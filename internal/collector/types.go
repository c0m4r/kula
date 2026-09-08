package collector

import "time"

// Sample holds all metrics collected at a single point in time.
type Sample struct {
	Timestamp time.Time `json:"ts"`

	CPU     CPUStats           `json:"cpu"`
	LoadAvg LoadAvg            `json:"lavg"`
	Memory  MemoryStats        `json:"mem"`
	Swap    SwapStats          `json:"swap"`
	Network NetworkStats       `json:"net"`
	Disks   DiskStats          `json:"disk"`
	System  SystemStats        `json:"sys"`
	Process ProcessStats       `json:"proc"`
	Self    SelfStats          `json:"self"`
	GPU     []GPUStats         `json:"gpu,omitempty"`
	PSU     []PowerSupplyStats `json:"psu,omitempty"`
	Apps    ApplicationsStats  `json:"apps,omitempty"`
}

// CPUStats holds per-core and total CPU usage percentages.
type CPUStats struct {
	Total       CPUCoreStats    `json:"total"`
	NumCores    int             `json:"num_cores" agg:"last"`
	Temperature float64         `json:"temp,omitempty" agg:"mean"`
	Sensors     []CPUTempSensor `json:"sensors,omitempty"`
}

type CPUTempSensor struct {
	Name  string  `json:"name" agg:"identity"`
	Value float64 `json:"value" agg:"mean"`
}

type CPUCoreStats struct {
	User    float64 `json:"user" agg:"mean"`
	System  float64 `json:"system" agg:"mean"`
	IOWait  float64 `json:"iowait" agg:"mean"`
	IRQ     float64 `json:"irq" agg:"mean"`
	SoftIRQ float64 `json:"softirq" agg:"mean"`
	Steal   float64 `json:"steal" agg:"mean"`
	Usage   float64 `json:"usage" agg:"mean"` // 100 - idle
}

type LoadAvg struct {
	Load1   float64 `json:"load1" agg:"mean"`
	Load5   float64 `json:"load5" agg:"mean"`
	Load15  float64 `json:"load15" agg:"mean"`
	Running int     `json:"running" agg:"mean"`
	Total   int     `json:"total" agg:"mean"`
}

type MemoryStats struct {
	Total       uint64  `json:"total" agg:"last"`
	Free        uint64  `json:"free" agg:"mean"`
	Available   uint64  `json:"available" agg:"mean"`
	Used        uint64  `json:"used" agg:"mean"`
	Buffers     uint64  `json:"buffers" agg:"mean"`
	Cached      uint64  `json:"cached" agg:"mean"`
	Shmem       uint64  `json:"shmem" agg:"mean"`
	UsedPercent float64 `json:"used_pct" agg:"mean"`
}

type SwapStats struct {
	Total       uint64  `json:"total" agg:"last"`
	Free        uint64  `json:"free" agg:"mean"`
	Used        uint64  `json:"used" agg:"mean"`
	UsedPercent float64 `json:"used_pct" agg:"mean"`
}

type NetworkStats struct {
	Interfaces []NetInterface `json:"ifaces"`
	TCP        TCPStats       `json:"tcp"`
	Sockets    SocketStats    `json:"sockets"`
}

type NetInterface struct {
	Name    string  `json:"name" agg:"identity"`
	RxBytes uint64  `json:"rx_bytes" agg:"last"`
	TxBytes uint64  `json:"tx_bytes" agg:"last"`
	RxMbps  float64 `json:"rx_mbps" agg:"mean"`
	TxMbps  float64 `json:"tx_mbps" agg:"mean"`
	RxPkts  uint64  `json:"rx_pkts" agg:"last"`
	TxPkts  uint64  `json:"tx_pkts" agg:"last"`
	RxPPS   float64 `json:"rx_pps" agg:"mean"`
	TxPPS   float64 `json:"tx_pps" agg:"mean"`
	RxErrs  uint64  `json:"rx_errs" agg:"last"`
	TxErrs  uint64  `json:"tx_errs" agg:"last"`
	RxDrop  uint64  `json:"rx_drop" agg:"last"`
	TxDrop  uint64  `json:"tx_drop" agg:"last"`
}

// TCPStats holds key TCP protocol counters.
// CurrEstab is a gauge. InErrs, OutRsts, and Retrans are per-second rates (delta/elapsed).
type TCPStats struct {
	CurrEstab uint64  `json:"curr_estab" agg:"mean"`
	InErrs    float64 `json:"in_errs_ps" agg:"mean"`
	OutRsts   float64 `json:"out_rsts_ps" agg:"mean"`
	Retrans   float64 `json:"retrans_ps" agg:"mean"`
}

type SocketStats struct {
	TCPInUse int `json:"tcp_inuse" agg:"mean"`
	TCPTw    int `json:"tcp_tw" agg:"mean"`
	UDPInUse int `json:"udp_inuse" agg:"mean"`
}

type DiskStats struct {
	Devices     []DiskDevice     `json:"devices"`
	FileSystems []FileSystemInfo `json:"filesystems"`
}

type DiskDevice struct {
	Name         string           `json:"name" agg:"identity"`
	ReadsPerSec  float64          `json:"reads_ps" agg:"mean"`
	WritesPerSec float64          `json:"writes_ps" agg:"mean"`
	ReadBytesPS  float64          `json:"read_bps" agg:"mean"`
	WriteBytesPS float64          `json:"write_bps" agg:"mean"`
	Utilization  float64          `json:"util_pct" agg:"mean"`
	Temperature  float64          `json:"temp,omitempty" agg:"mean"`
	Sensors      []DiskTempSensor `json:"sensors,omitempty"`
}

type DiskTempSensor struct {
	Name  string  `json:"name" agg:"identity"`
	Value float64 `json:"value" agg:"mean"`
}

type FileSystemInfo struct {
	Device     string  `json:"device"`
	MountPoint string  `json:"mount" agg:"identity"`
	FSType     string  `json:"fstype"`
	Total      uint64  `json:"total" agg:"last"`
	Used       uint64  `json:"used" agg:"mean"`
	Available  uint64  `json:"available" agg:"mean"`
	UsedPct    float64 `json:"used_pct" agg:"mean"`
}

type SystemStats struct {
	Hostname    string  `json:"hostname"`
	Uptime      float64 `json:"uptime_sec" agg:"last"`
	UptimeHuman string  `json:"uptime_human"`
	Entropy     int     `json:"entropy" agg:"mean"`
	ClockSync   bool    `json:"clock_synced"`
	ClockSource string  `json:"clock_source"`
	UserCount   int     `json:"user_count" agg:"mean"`
}

type ProcessStats struct {
	Total    int `json:"total" agg:"mean"`
	Running  int `json:"running" agg:"mean"`
	Sleeping int `json:"sleeping" agg:"mean"`
	Zombie   int `json:"zombie" agg:"mean"`
	Blocked  int `json:"blocked" agg:"mean"`
	Threads  int `json:"threads" agg:"mean"`
}

type SelfStats struct {
	CPUPercent float64 `json:"cpu_pct" agg:"mean"`
	MemRSS     uint64  `json:"mem_rss" agg:"mean"`
	FDs        int     `json:"fds" agg:"mean"`
}

type GPUStats struct {
	Index       int     `json:"index" agg:"identity"`
	Name        string  `json:"name" agg:"identity_fallback"`
	Driver      string  `json:"driver"`
	Temperature float64 `json:"temp,omitempty" agg:"mean"`
	VRAMUsed    uint64  `json:"vram_used,omitempty" agg:"mean"`
	VRAMTotal   uint64  `json:"vram_total,omitempty" agg:"last"`
	VRAMUsedPct float64 `json:"vram_pct,omitempty" agg:"mean"`
	LoadPct     float64 `json:"load_pct,omitempty" agg:"mean"`
	PowerW      float64 `json:"power_w,omitempty" agg:"mean"`
}

// ApplicationsStats holds metrics from external applications.
type ApplicationsStats struct {
	Nginx      *NginxStats                    `json:"nginx,omitempty"`
	Apache2    *Apache2Stats                  `json:"apache2,omitempty"`
	Containers []ContainerStats               `json:"containers,omitempty"`
	Postgres   *PostgresStats                 `json:"postgres,omitempty"`
	Mysql      *MysqlStats                    `json:"mysql,omitempty"`
	Custom     map[string][]CustomMetricValue `json:"custom,omitempty"`
}

// NginxStats holds metrics parsed from the nginx stub_status module.
type NginxStats struct {
	ActiveConnections int     `json:"active_conn" agg:"mean"`
	Accepts           uint64  `json:"accepts" agg:"last"`
	Handled           uint64  `json:"handled" agg:"last"`
	Requests          uint64  `json:"requests" agg:"last"`
	AcceptsPS         float64 `json:"accepts_ps" agg:"mean"`
	HandledPS         float64 `json:"handled_ps" agg:"mean"`
	RequestsPS        float64 `json:"requests_ps" agg:"mean"`
	Reading           int     `json:"reading" agg:"mean"`
	Writing           int     `json:"writing" agg:"mean"`
	Waiting           int     `json:"waiting" agg:"mean"`
}

// Apache2Stats holds metrics parsed from the Apache2 mod_status ?auto endpoint.
type Apache2Stats struct {
	BusyWorkers   int     `json:"busy_workers" agg:"mean"`
	IdleWorkers   int     `json:"idle_workers" agg:"mean"`
	TotalAccesses uint64  `json:"total_accesses" agg:"last"`
	TotalKBytes   uint64  `json:"total_kbytes" agg:"last"`
	AccessesPS    float64 `json:"accesses_ps" agg:"mean"`
	KBytesPS      float64 `json:"kbytes_ps" agg:"mean"`
	ReqPerSec     float64 `json:"req_per_sec" agg:"mean"`
	BytesPerSec   float64 `json:"bytes_per_sec" agg:"mean"`
	BytesPerReq   float64 `json:"bytes_per_req" agg:"mean"`
	CPULoad       float64 `json:"cpu_load" agg:"mean"`
	Uptime        int64   `json:"uptime" agg:"last"`
	Waiting       int     `json:"waiting" agg:"mean"`
	Reading       int     `json:"reading" agg:"mean"`
	Sending       int     `json:"sending" agg:"mean"`
	Keepalive     int     `json:"keepalive" agg:"mean"`
	Starting      int     `json:"starting" agg:"mean"`
	DNS           int     `json:"dns" agg:"mean"`
	Closing       int     `json:"closing" agg:"mean"`
	Logging       int     `json:"logging" agg:"mean"`
	Graceful      int     `json:"graceful" agg:"mean"`
	IdleCleanup   int     `json:"idle_cleanup" agg:"mean"`
	OpenSlots     int     `json:"open_slots" agg:"mean"`
}

// ContainerStats holds per-container resource usage metrics.
type ContainerStats struct {
	ID       string  `json:"id" agg:"identity_fallback"`
	Name     string  `json:"name" agg:"identity"`
	CPUPct   float64 `json:"cpu_pct" agg:"mean"`
	MemUsed  uint64  `json:"mem_used" agg:"mean"`
	MemLimit uint64  `json:"mem_limit" agg:"last"`
	MemPct   float64 `json:"mem_pct" agg:"mean"`
	NetRxBPS float64 `json:"net_rx_bps" agg:"mean"`
	NetTxBPS float64 `json:"net_tx_bps" agg:"mean"`
	DiskRBPS float64 `json:"disk_r_bps" agg:"mean"`
	DiskWBPS float64 `json:"disk_w_bps" agg:"mean"`
}

// PostgresStats holds PostgreSQL database metrics.
type PostgresStats struct {
	// Connection state (from pg_stat_activity)
	ActiveConns   int `json:"active_conns" agg:"mean"`
	IdleConns     int `json:"idle_conns" agg:"mean"`
	IdleInTxConns int `json:"idle_in_tx_conns" agg:"mean"`
	WaitingConns  int `json:"waiting_conns" agg:"mean"`
	MaxConns      int `json:"max_conns" agg:"last"`

	// Transaction throughput (per-second rates from pg_stat_database)
	TxCommitPS   float64 `json:"tx_commit_ps" agg:"mean"`
	TxRollbackPS float64 `json:"tx_rollback_ps" agg:"mean"`

	// Tuple (row) activity rates
	TupFetchedPS  float64 `json:"tup_fetched_ps" agg:"mean"`
	TupReturnedPS float64 `json:"tup_returned_ps" agg:"mean"`
	TupInsertedPS float64 `json:"tup_inserted_ps" agg:"mean"`
	TupUpdatedPS  float64 `json:"tup_updated_ps" agg:"mean"`
	TupDeletedPS  float64 `json:"tup_deleted_ps" agg:"mean"`

	// I/O: raw block rates and derived cache hit ratio
	BlksReadPS float64 `json:"blks_read_ps" agg:"mean"`
	BlksHitPS  float64 `json:"blks_hit_ps" agg:"mean"`
	BlksHitPct float64 `json:"blks_hit_pct" agg:"mean"`

	// Locking
	DeadlocksPS float64 `json:"deadlocks_ps" agg:"mean"`

	// Table health (from pg_stat_user_tables)
	DeadTuples      int64 `json:"dead_tuples" agg:"mean"`
	LiveTuples      int64 `json:"live_tuples" agg:"mean"`
	AutovacuumCount int64 `json:"autovacuum_count" agg:"last"`

	// Background writer rates (from pg_stat_bgwriter)
	BufCheckpointPS float64 `json:"buf_checkpoint_ps" agg:"mean"`
	BufBackendPS    float64 `json:"buf_backend_ps" agg:"mean"`

	// Database size
	DBSizeBytes int64 `json:"db_size_bytes" agg:"mean"`

	// Replication. IsInRecovery is true on a standby. ReplicaCount is
	// meaningful on a primary (from pg_stat_replication). The two lag fields
	// are meaningful on a standby; on a primary they are 0.
	IsInRecovery          bool    `json:"is_in_recovery"`
	ReplicaCount          int     `json:"replica_count" agg:"mean"`
	ReplicationLagBytes   int64   `json:"repl_lag_bytes" agg:"mean"`
	ReplicationLagSeconds float64 `json:"repl_lag_seconds" agg:"mean"`
}

// MysqlStats holds MySQL database metrics.
type MysqlStats struct {
	ThreadsConnected int `json:"threads_connected" agg:"mean"`
	ThreadsRunning   int `json:"threads_running" agg:"mean"`
	ThreadsCached    int `json:"threads_cached" agg:"mean"`
	MaxConnections   int `json:"max_conns" agg:"last"`

	QueriesPS   float64 `json:"queries_ps" agg:"mean"`
	ComSelectPS float64 `json:"select_ps" agg:"mean"`
	ComInsertPS float64 `json:"insert_ps" agg:"mean"`
	ComUpdatePS float64 `json:"update_ps" agg:"mean"`
	ComDeletePS float64 `json:"delete_ps" agg:"mean"`

	SlowQueriesPS float64 `json:"slow_queries_ps" agg:"mean"`

	InnodbBufferPoolHitPct float64 `json:"innodb_buffer_pool_hit_pct" agg:"mean"`
	InnodbBPReadsPS        float64 `json:"innodb_bp_reads_ps" agg:"mean"`

	TableLocksWaitedPS float64 `json:"table_locks_waited_ps" agg:"mean"`
	RowLockWaitsPS     float64 `json:"row_lock_waits_ps" agg:"mean"`

	// Replication. ReplicaIORunning/ReplicaSQLRunning come from SHOW REPLICA
	// STATUS (or SHOW SLAVE STATUS) and are false when the server isn't
	// configured as a replica. ReplicaSecondsBehind uses -1 to mean NULL or
	// not-replicating. ReplicaCount is from SHOW REPLICAS / SHOW SLAVE HOSTS
	// and is meaningful on a primary.
	ReplicaIORunning     bool `json:"replica_io_running"`
	ReplicaSQLRunning    bool `json:"replica_sql_running"`
	ReplicaSecondsBehind int  `json:"replica_seconds_behind" agg:"mean_nonnegative"`
	ReplicaCount         int  `json:"replica_count" agg:"mean"`

	// Replication error / state. LastIOErrno and LastSQLErrno are the most
	// recent MySQL error codes reported by the IO and SQL threads; 0 means
	// no error. IOState is Slave_IO_State (e.g. "Waiting for master to send
	// event") — useful for distinguishing "stalled but technically running"
	// from "actively waiting for data". Stored capped at 200 bytes.
	LastIOErrno  int    `json:"replica_last_io_errno" agg:"last"`
	LastSQLErrno int    `json:"replica_last_sql_errno" agg:"last"`
	IOState      string `json:"replica_io_state,omitempty"`
}

// PowerSupplyStats holds metrics for a single power supply (battery, mains adapter, UPS).
type PowerSupplyStats struct {
	Name         string  `json:"name" agg:"identity"`
	Type         string  `json:"type"`                          // "Battery", "Mains", "UPS"
	Status       string  `json:"status"`                        // "Charging", "Discharging", "Full", "Not charging"
	Capacity     int     `json:"capacity,omitempty" agg:"mean"` // 0-100%
	VoltageV     float64 `json:"voltage_v,omitempty" agg:"mean"`
	CurrentA     float64 `json:"current_a,omitempty" agg:"mean"`
	PowerW       float64 `json:"power_w,omitempty" agg:"mean"`
	EnergyWhNow  float64 `json:"energy_wh_now,omitempty" agg:"mean"`
	EnergyWhFull float64 `json:"energy_wh_full,omitempty" agg:"last"`
}

// CustomMetricValue holds a single named metric value from external input.
type CustomMetricValue struct {
	Name  string  `json:"name" agg:"identity"`
	Value float64 `json:"value" agg:"mean"`
}
