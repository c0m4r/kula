package web

import (
	"crypto/subtle"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"kula/internal/collector"
)

// handleMetrics serves Prometheus-compatible metrics in text exposition format.
// The endpoint is disabled unless explicitly registered by configuration.
// When a bearer token is configured, callers must send
// Authorization: Bearer <token>.
// It reflects the latest collected sample; if no sample is available yet it
// returns an empty 200 response so scrapers don't alarm on startup.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	if token := s.cfg.PrometheusMetrics.Token; token != "" {
		authz := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(authz, prefix) || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(authz[len(prefix):])), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="kula-metrics"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	sample, err := s.store.QueryLatest()
	if err != nil || sample == nil || sample.Data == nil {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}

	d := sample.Data
	hostname := d.System.Hostname
	if hostname == "" {
		hostname = s.global.Hostname
	}

	var b strings.Builder
	b.Grow(4096)

	// Helper closures
	gauge := func(name, help, labels string, value float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		if labels != "" {
			fmt.Fprintf(&b, "%s{%s} %s\n", name, labels, formatPrometheusValue(value))
		} else {
			fmt.Fprintf(&b, "%s %s\n", name, formatPrometheusValue(value))
		}
	}
	counter := func(name, help, labels string, value float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
		if labels != "" {
			fmt.Fprintf(&b, "%s{%s} %s\n", name, labels, formatPrometheusValue(value))
		} else {
			fmt.Fprintf(&b, "%s %s\n", name, formatPrometheusValue(value))
		}
	}

	lbl := func(k, v string) string { return k + "=\"" + escapeLabel(v) + "\"" }
	host := lbl("host", hostname)

	// ---- CPU ---------------------------------------------------------------
	gauge("kula_cpu_usage_percent",
		"Total CPU usage percentage (100 - idle).",
		host, d.CPU.Total.Usage)
	gauge("kula_cpu_user_percent",
		"CPU time spent in user mode.",
		host, d.CPU.Total.User)
	gauge("kula_cpu_system_percent",
		"CPU time spent in kernel mode.",
		host, d.CPU.Total.System)
	gauge("kula_cpu_iowait_percent",
		"CPU time spent waiting for I/O.",
		host, d.CPU.Total.IOWait)
	gauge("kula_cpu_irq_percent",
		"CPU time spent servicing hardware interrupts.",
		host, d.CPU.Total.IRQ)
	gauge("kula_cpu_softirq_percent",
		"CPU time spent servicing software interrupts.",
		host, d.CPU.Total.SoftIRQ)
	gauge("kula_cpu_steal_percent",
		"CPU time stolen by the hypervisor.",
		host, d.CPU.Total.Steal)
	gauge("kula_cpu_cores",
		"Number of logical CPU cores.",
		host, float64(d.CPU.NumCores))
	if d.CPU.Temperature > 0 {
		gauge("kula_cpu_temperature_celsius",
			"CPU package temperature in degrees Celsius.",
			host, d.CPU.Temperature)
	}
	for _, sens := range d.CPU.Sensors {
		gauge("kula_cpu_sensor_temperature_celsius",
			"Per-sensor CPU temperature in degrees Celsius.",
			host+","+lbl("sensor", sens.Name), sens.Value)
	}

	// ---- Load average ------------------------------------------------------
	gauge("kula_load_average_1m",
		"System load average over the last 1 minute.",
		host, d.LoadAvg.Load1)
	gauge("kula_load_average_5m",
		"System load average over the last 5 minutes.",
		host, d.LoadAvg.Load5)
	gauge("kula_load_average_15m",
		"System load average over the last 15 minutes.",
		host, d.LoadAvg.Load15)
	gauge("kula_processes_running",
		"Number of processes currently running.",
		host, float64(d.LoadAvg.Running))
	gauge("kula_processes_total",
		"Total number of processes.",
		host, float64(d.LoadAvg.Total))

	// ---- Memory ------------------------------------------------------------
	gauge("kula_memory_total_bytes",
		"Total installed physical memory in bytes.",
		host, float64(d.Memory.Total))
	gauge("kula_memory_used_bytes",
		"Used physical memory in bytes.",
		host, float64(d.Memory.Used))
	gauge("kula_memory_free_bytes",
		"Free physical memory in bytes.",
		host, float64(d.Memory.Free))
	gauge("kula_memory_available_bytes",
		"Available memory (free + reclaimable caches) in bytes.",
		host, float64(d.Memory.Available))
	gauge("kula_memory_buffers_bytes",
		"Memory used for kernel buffers in bytes.",
		host, float64(d.Memory.Buffers))
	gauge("kula_memory_cached_bytes",
		"Memory used for page cache in bytes.",
		host, float64(d.Memory.Cached))
	gauge("kula_memory_shmem_bytes",
		"Shared memory in bytes.",
		host, float64(d.Memory.Shmem))
	gauge("kula_memory_used_percent",
		"Used physical memory as a percentage of total.",
		host, d.Memory.UsedPercent)

	// ---- Swap --------------------------------------------------------------
	gauge("kula_swap_total_bytes",
		"Total swap space in bytes.",
		host, float64(d.Swap.Total))
	gauge("kula_swap_used_bytes",
		"Used swap space in bytes.",
		host, float64(d.Swap.Used))
	gauge("kula_swap_free_bytes",
		"Free swap space in bytes.",
		host, float64(d.Swap.Free))
	gauge("kula_swap_used_percent",
		"Used swap as a percentage of total swap.",
		host, d.Swap.UsedPercent)

	// ---- Network -----------------------------------------------------------
	for _, iface := range d.Network.Interfaces {
		ifLbl := host + "," + lbl("interface", iface.Name)
		gauge("kula_network_rx_mbps",
			"Network receive throughput in Mbps.",
			ifLbl, iface.RxMbps)
		gauge("kula_network_tx_mbps",
			"Network transmit throughput in Mbps.",
			ifLbl, iface.TxMbps)
		gauge("kula_network_rx_packets_per_second",
			"Network receive packet rate.",
			ifLbl, iface.RxPPS)
		gauge("kula_network_tx_packets_per_second",
			"Network transmit packet rate.",
			ifLbl, iface.TxPPS)
		counter("kula_network_rx_bytes_total",
			"Total bytes received on the interface.",
			ifLbl, float64(iface.RxBytes))
		counter("kula_network_tx_bytes_total",
			"Total bytes transmitted on the interface.",
			ifLbl, float64(iface.TxBytes))
		counter("kula_network_rx_packets_total",
			"Total packets received on the interface.",
			ifLbl, float64(iface.RxPkts))
		counter("kula_network_tx_packets_total",
			"Total packets transmitted on the interface.",
			ifLbl, float64(iface.TxPkts))
		counter("kula_network_rx_errors_total",
			"Total receive errors on the interface.",
			ifLbl, float64(iface.RxErrs))
		counter("kula_network_tx_errors_total",
			"Total transmit errors on the interface.",
			ifLbl, float64(iface.TxErrs))
		counter("kula_network_rx_drops_total",
			"Total receive drops on the interface.",
			ifLbl, float64(iface.RxDrop))
		counter("kula_network_tx_drops_total",
			"Total transmit drops on the interface.",
			ifLbl, float64(iface.TxDrop))
	}
	gauge("kula_tcp_established",
		"Number of TCP connections in ESTABLISHED state.",
		host, float64(d.Network.TCP.CurrEstab))
	gauge("kula_tcp_errors_per_second",
		"TCP input errors per second.",
		host, d.Network.TCP.InErrs)
	gauge("kula_tcp_resets_per_second",
		"TCP resets sent per second.",
		host, d.Network.TCP.OutRsts)
	gauge("kula_sockets_tcp_in_use",
		"Number of TCP sockets in use.",
		host, float64(d.Network.Sockets.TCPInUse))
	gauge("kula_sockets_tcp_time_wait",
		"Number of TCP sockets in TIME_WAIT state.",
		host, float64(d.Network.Sockets.TCPTw))
	gauge("kula_sockets_udp_in_use",
		"Number of UDP sockets in use.",
		host, float64(d.Network.Sockets.UDPInUse))

	// ---- Disk I/O ----------------------------------------------------------
	for _, dev := range d.Disks.Devices {
		devLbl := host + "," + lbl("device", dev.SeriesKey())
		gauge("kula_disk_info", "Disk identity and current kernel name (kernel source is unstable).",
			devLbl+","+lbl("kernel_name", dev.Name)+","+lbl("identity_source", dev.IdentitySource()), 1)
		gauge("kula_disk_reads_per_second",
			"Disk read operations per second.",
			devLbl, dev.ReadsPerSec)
		gauge("kula_disk_writes_per_second",
			"Disk write operations per second.",
			devLbl, dev.WritesPerSec)
		gauge("kula_disk_read_bytes_per_second",
			"Disk read throughput in bytes per second.",
			devLbl, dev.ReadBytesPS)
		gauge("kula_disk_write_bytes_per_second",
			"Disk write throughput in bytes per second.",
			devLbl, dev.WriteBytesPS)
		gauge("kula_disk_utilization_percent",
			"Disk utilization as a percentage of time the device is busy.",
			devLbl, dev.Utilization)
		if dev.Temperature > 0 {
			gauge("kula_disk_temperature_celsius",
				"Disk drive temperature in degrees Celsius.",
				devLbl, dev.Temperature)
		}
	}

	// ---- Filesystems -------------------------------------------------------
	for _, fs := range d.Disks.FileSystems {
		fsLbl := host + "," + lbl("device", fs.Device) + "," + lbl("mountpoint", fs.MountPoint) + "," + lbl("fstype", fs.FSType)
		gauge("kula_filesystem_size_bytes",
			"Total filesystem size in bytes.",
			fsLbl, float64(fs.Total))
		gauge("kula_filesystem_used_bytes",
			"Used filesystem space in bytes.",
			fsLbl, float64(fs.Used))
		gauge("kula_filesystem_available_bytes",
			"Available filesystem space in bytes.",
			fsLbl, float64(fs.Available))
		gauge("kula_filesystem_used_percent",
			"Used filesystem space as a percentage of total.",
			fsLbl, fs.UsedPct)
	}

	// ---- System ------------------------------------------------------------
	gauge("kula_system_uptime_seconds",
		"System uptime in seconds.",
		host, d.System.Uptime)
	gauge("kula_system_entropy_available",
		"Available entropy in the kernel entropy pool.",
		host, float64(d.System.Entropy))
	clockSync := 0.0
	if d.System.ClockSync {
		clockSync = 1.0
	}
	gauge("kula_system_clock_synced",
		"1 if the system clock is synchronized (NTP/PTP), 0 otherwise.",
		host, clockSync)
	gauge("kula_system_logged_in_users",
		"Number of users currently logged in.",
		host, float64(d.System.UserCount))

	// ---- Processes ---------------------------------------------------------
	gauge("kula_processes_sleeping",
		"Number of sleeping processes.",
		host, float64(d.Process.Sleeping))
	gauge("kula_processes_zombie",
		"Number of zombie processes.",
		host, float64(d.Process.Zombie))
	gauge("kula_processes_blocked",
		"Number of processes blocked on I/O.",
		host, float64(d.Process.Blocked))
	gauge("kula_threads_total",
		"Total number of threads across all processes.",
		host, float64(d.Process.Threads))

	// ---- GPU ---------------------------------------------------------------
	for _, gpu := range d.GPU {
		gpuLbl := host + "," + lbl("index", fmt.Sprintf("%d", gpu.Index)) + "," + lbl("name", gpu.Name)
		if gpu.Temperature > 0 {
			gauge("kula_gpu_temperature_celsius",
				"GPU temperature in degrees Celsius.",
				gpuLbl, gpu.Temperature)
		}
		if gpu.LoadPct > 0 {
			gauge("kula_gpu_load_percent",
				"GPU compute load as a percentage.",
				gpuLbl, gpu.LoadPct)
		}
		if gpu.PowerW > 0 {
			gauge("kula_gpu_power_watts",
				"GPU power consumption in watts.",
				gpuLbl, gpu.PowerW)
		}
		if gpu.VRAMTotal > 0 {
			gauge("kula_gpu_vram_total_bytes",
				"Total GPU VRAM in bytes.",
				gpuLbl, float64(gpu.VRAMTotal))
			gauge("kula_gpu_vram_used_bytes",
				"Used GPU VRAM in bytes.",
				gpuLbl, float64(gpu.VRAMUsed))
			gauge("kula_gpu_vram_used_percent",
				"Used GPU VRAM as a percentage of total.",
				gpuLbl, gpu.VRAMUsedPct)
		}
	}

	// ---- Power supplies ----------------------------------------------------
	for _, psu := range d.PSU {
		psuLbl := host + "," + lbl("name", psu.Name) + "," + lbl("type", psu.Type) + "," + lbl("status", psu.Status)
		if psu.Capacity > 0 {
			gauge("kula_psu_capacity_percent",
				"Power supply charge level as a percentage.",
				psuLbl, float64(psu.Capacity))
		}
		if psu.VoltageV != 0 {
			gauge("kula_psu_voltage_volts",
				"Power supply voltage in volts.",
				psuLbl, psu.VoltageV)
		}
		if psu.CurrentA != 0 {
			gauge("kula_psu_current_amperes",
				"Power supply current in amperes.",
				psuLbl, psu.CurrentA)
		}
		if psu.PowerW != 0 {
			gauge("kula_psu_power_watts",
				"Power supply power draw in watts.",
				psuLbl, psu.PowerW)
		}
		if psu.EnergyWhFull > 0 {
			gauge("kula_psu_energy_now_wh",
				"Current stored energy in watt-hours.",
				psuLbl, psu.EnergyWhNow)
			gauge("kula_psu_energy_full_wh",
				"Full-charge energy capacity in watt-hours.",
				psuLbl, psu.EnergyWhFull)
		}
	}

	// ---- Applications: Nginx -----------------------------------------------
	if n := d.Apps.Nginx; n != nil {
		gauge("kula_nginx_active_connections",
			"Nginx active client connections.",
			host, float64(n.ActiveConnections))
		gauge("kula_nginx_reading",
			"Nginx connections reading request headers.",
			host, float64(n.Reading))
		gauge("kula_nginx_writing",
			"Nginx connections writing response back to client.",
			host, float64(n.Writing))
		gauge("kula_nginx_waiting",
			"Nginx idle keep-alive connections.",
			host, float64(n.Waiting))
		gauge("kula_nginx_accepts_per_second",
			"Nginx accepted connections per second.",
			host, n.AcceptsPS)
		gauge("kula_nginx_handled_per_second",
			"Nginx handled connections per second.",
			host, n.HandledPS)
		gauge("kula_nginx_requests_per_second",
			"Nginx client requests per second.",
			host, n.RequestsPS)
		counter("kula_nginx_accepts_total",
			"Total accepted client connections since Nginx start.",
			host, float64(n.Accepts))
		counter("kula_nginx_handled_total",
			"Total handled client connections since Nginx start.",
			host, float64(n.Handled))
		counter("kula_nginx_requests_total",
			"Total client requests since Nginx start.",
			host, float64(n.Requests))
	}

	// ---- Applications: Apache2 ---------------------------------------------
	if a := d.Apps.Apache2; a != nil {
		gauge("kula_apache2_busy_workers",
			"Apache2 workers currently handling requests.",
			host, float64(a.BusyWorkers))
		gauge("kula_apache2_idle_workers",
			"Apache2 idle workers available to serve requests.",
			host, float64(a.IdleWorkers))
		gauge("kula_apache2_open_slots",
			"Apache2 open (unused) worker slots.",
			host, float64(a.OpenSlots))
		gauge("kula_apache2_cpu_load",
			"Apache2 server CPU load (percentage).",
			host, a.CPULoad)
		gauge("kula_apache2_uptime_seconds",
			"Apache2 server uptime in seconds.",
			host, float64(a.Uptime))
		gauge("kula_apache2_requests_per_second",
			"Apache2 requests served per second.",
			host, a.ReqPerSec)
		gauge("kula_apache2_bytes_per_second",
			"Apache2 bytes served per second.",
			host, a.BytesPerSec)
		gauge("kula_apache2_bytes_per_request",
			"Average response size in bytes per Apache2 request.",
			host, a.BytesPerReq)
		counter("kula_apache2_accesses_total",
			"Total Apache2 accesses since server start.",
			host, float64(a.TotalAccesses))
		counter("kula_apache2_kbytes_total",
			"Total kilobytes served by Apache2 since server start.",
			host, float64(a.TotalKBytes))
		for state, val := range map[string]int{
			"waiting":      a.Waiting,
			"reading":      a.Reading,
			"sending":      a.Sending,
			"keepalive":    a.Keepalive,
			"starting":     a.Starting,
			"dns":          a.DNS,
			"closing":      a.Closing,
			"logging":      a.Logging,
			"graceful":     a.Graceful,
			"idle_cleanup": a.IdleCleanup,
		} {
			gauge("kula_apache2_scoreboard",
				"Number of Apache2 workers in a given scoreboard state.",
				host+","+lbl("state", state), float64(val))
		}
	}

	// ---- Applications: Containers ------------------------------------------
	for _, c := range d.Apps.Containers {
		cLbl := host + "," + lbl("id", c.ID) + "," + lbl("name", c.Name)
		gauge("kula_container_cpu_percent",
			"Container CPU usage as a percentage.",
			cLbl, c.CPUPct)
		gauge("kula_container_memory_used_bytes",
			"Container memory usage in bytes.",
			cLbl, float64(c.MemUsed))
		gauge("kula_container_memory_limit_bytes",
			"Container memory limit in bytes.",
			cLbl, float64(c.MemLimit))
		gauge("kula_container_memory_used_percent",
			"Container memory usage as a percentage of its limit.",
			cLbl, c.MemPct)
		gauge("kula_container_network_rx_bytes_per_second",
			"Container network receive throughput in bytes per second.",
			cLbl, c.NetRxBPS)
		gauge("kula_container_network_tx_bytes_per_second",
			"Container network transmit throughput in bytes per second.",
			cLbl, c.NetTxBPS)
		gauge("kula_container_disk_read_bytes_per_second",
			"Container block I/O read throughput in bytes per second.",
			cLbl, c.DiskRBPS)
		gauge("kula_container_disk_write_bytes_per_second",
			"Container block I/O write throughput in bytes per second.",
			cLbl, c.DiskWBPS)
	}

	// ---- Applications: PostgreSQL ------------------------------------------
	if p := d.Apps.Postgres; p != nil {
		gauge("kula_postgres_connections_active",
			"PostgreSQL connections in the active state.",
			host, float64(p.ActiveConns))
		gauge("kula_postgres_connections_idle",
			"PostgreSQL connections in the idle state.",
			host, float64(p.IdleConns))
		gauge("kula_postgres_connections_idle_in_transaction",
			"PostgreSQL connections idle inside an open transaction.",
			host, float64(p.IdleInTxConns))
		gauge("kula_postgres_connections_waiting",
			"PostgreSQL connections waiting on a lock or event.",
			host, float64(p.WaitingConns))
		gauge("kula_postgres_connections_max",
			"PostgreSQL configured maximum number of connections.",
			host, float64(p.MaxConns))
		gauge("kula_postgres_transactions_committed_per_second",
			"PostgreSQL committed transactions per second.",
			host, p.TxCommitPS)
		gauge("kula_postgres_transactions_rolled_back_per_second",
			"PostgreSQL rolled-back transactions per second.",
			host, p.TxRollbackPS)
		gauge("kula_postgres_tuples_fetched_per_second",
			"PostgreSQL tuples fetched per second.",
			host, p.TupFetchedPS)
		gauge("kula_postgres_tuples_returned_per_second",
			"PostgreSQL tuples returned per second.",
			host, p.TupReturnedPS)
		gauge("kula_postgres_tuples_inserted_per_second",
			"PostgreSQL tuples inserted per second.",
			host, p.TupInsertedPS)
		gauge("kula_postgres_tuples_updated_per_second",
			"PostgreSQL tuples updated per second.",
			host, p.TupUpdatedPS)
		gauge("kula_postgres_tuples_deleted_per_second",
			"PostgreSQL tuples deleted per second.",
			host, p.TupDeletedPS)
		gauge("kula_postgres_blocks_read_per_second",
			"PostgreSQL disk block reads per second.",
			host, p.BlksReadPS)
		gauge("kula_postgres_blocks_hit_per_second",
			"PostgreSQL buffer cache hits per second.",
			host, p.BlksHitPS)
		gauge("kula_postgres_buffer_cache_hit_percent",
			"PostgreSQL buffer cache hit ratio as a percentage.",
			host, p.BlksHitPct)
		gauge("kula_postgres_deadlocks_per_second",
			"PostgreSQL deadlocks detected per second.",
			host, p.DeadlocksPS)
		gauge("kula_postgres_dead_tuples",
			"PostgreSQL dead tuples awaiting vacuum.",
			host, float64(p.DeadTuples))
		gauge("kula_postgres_live_tuples",
			"PostgreSQL live (estimated) tuples.",
			host, float64(p.LiveTuples))
		gauge("kula_postgres_autovacuum_count",
			"PostgreSQL autovacuum runs against user tables.",
			host, float64(p.AutovacuumCount))
		gauge("kula_postgres_buffers_checkpoint_per_second",
			"PostgreSQL buffers written by checkpoints per second.",
			host, p.BufCheckpointPS)
		gauge("kula_postgres_buffers_backend_per_second",
			"PostgreSQL buffers written by backends per second.",
			host, p.BufBackendPS)
		gauge("kula_postgres_database_size_bytes",
			"PostgreSQL monitored database size in bytes.",
			host, float64(p.DBSizeBytes))
		inRecovery := 0.0
		if p.IsInRecovery {
			inRecovery = 1.0
		}
		gauge("kula_postgres_is_in_recovery",
			"1 if this PostgreSQL node is operating as a standby, 0 if primary.",
			host, inRecovery)
		gauge("kula_postgres_replicas_connected",
			"Number of standbys connected to this PostgreSQL primary.",
			host, float64(p.ReplicaCount))
		gauge("kula_postgres_replication_lag_bytes",
			"PostgreSQL WAL bytes behind primary on a standby (0 on a primary).",
			host, float64(p.ReplicationLagBytes))
		gauge("kula_postgres_replication_lag_seconds",
			"PostgreSQL replay lag in seconds on a standby (0 on a primary).",
			host, p.ReplicationLagSeconds)
	}

	// ---- Applications: MySQL/MariaDB ---------------------------------------
	if m := d.Apps.Mysql; m != nil {
		gauge("kula_mysql_threads_connected",
			"MySQL client threads currently connected.",
			host, float64(m.ThreadsConnected))
		gauge("kula_mysql_threads_running",
			"MySQL client threads currently running queries.",
			host, float64(m.ThreadsRunning))
		gauge("kula_mysql_threads_cached",
			"MySQL threads in the thread cache.",
			host, float64(m.ThreadsCached))
		gauge("kula_mysql_max_connections",
			"MySQL configured maximum number of client connections.",
			host, float64(m.MaxConnections))
		gauge("kula_mysql_queries_per_second",
			"MySQL queries executed per second.",
			host, m.QueriesPS)
		gauge("kula_mysql_select_per_second",
			"MySQL SELECT statements per second.",
			host, m.ComSelectPS)
		gauge("kula_mysql_insert_per_second",
			"MySQL INSERT statements per second.",
			host, m.ComInsertPS)
		gauge("kula_mysql_update_per_second",
			"MySQL UPDATE statements per second.",
			host, m.ComUpdatePS)
		gauge("kula_mysql_delete_per_second",
			"MySQL DELETE statements per second.",
			host, m.ComDeletePS)
		gauge("kula_mysql_slow_queries_per_second",
			"MySQL slow queries per second.",
			host, m.SlowQueriesPS)
		gauge("kula_mysql_innodb_buffer_pool_hit_percent",
			"MySQL InnoDB buffer pool hit ratio as a percentage.",
			host, m.InnodbBufferPoolHitPct)
		gauge("kula_mysql_innodb_buffer_pool_reads_per_second",
			"MySQL InnoDB buffer pool disk reads per second.",
			host, m.InnodbBPReadsPS)
		gauge("kula_mysql_table_locks_waited_per_second",
			"MySQL table lock waits per second.",
			host, m.TableLocksWaitedPS)
		gauge("kula_mysql_row_lock_waits_per_second",
			"MySQL row lock waits per second.",
			host, m.RowLockWaitsPS)
		ioRunning := 0.0
		if m.ReplicaIORunning {
			ioRunning = 1.0
		}
		sqlRunning := 0.0
		if m.ReplicaSQLRunning {
			sqlRunning = 1.0
		}
		gauge("kula_mysql_replica_io_running",
			"1 if the MySQL replica IO thread is running, 0 otherwise.",
			host, ioRunning)
		gauge("kula_mysql_replica_sql_running",
			"1 if the MySQL replica SQL thread is running, 0 otherwise.",
			host, sqlRunning)
		// -1 sentinel means the server isn't configured as a replica or the
		// value is NULL. Omit the gauge in that case so dashboards don't see
		// a misleading zero.
		if m.ReplicaSecondsBehind >= 0 {
			gauge("kula_mysql_replica_seconds_behind",
				"MySQL replica lag in seconds (Seconds_Behind_Source).",
				host, float64(m.ReplicaSecondsBehind))
		}
		gauge("kula_mysql_replicas_connected",
			"Number of replicas connected to this MySQL primary.",
			host, float64(m.ReplicaCount))
		// Errno gauges are 0 when there is no error. Non-zero values map
		// directly to a MySQL error code (e.g. 1236 = master log not found,
		// 1062 = duplicate entry from divergent applier).
		gauge("kula_mysql_replica_last_io_errno",
			"Last MySQL IO thread error code (0 = no error). Cf. Last_IO_Errno.",
			host, float64(m.LastIOErrno))
		gauge("kula_mysql_replica_last_sql_errno",
			"Last MySQL SQL thread error code (0 = no error). Cf. Last_SQL_Errno.",
			host, float64(m.LastSQLErrno))
		// Slave_IO_State as a Prometheus info-style metric (value always 1,
		// state surfaced as a label). The values are well-known and bounded
		// ("Waiting for master to send event", "Reconnecting after a failed
		// master event read", "Queueing master event to the relay log", …).
		// Emitted only when non-empty so non-replica servers don't add a
		// useless info line.
		if m.IOState != "" {
			gauge("kula_mysql_replica_io_state_info",
				"MySQL IO thread state (Slave_IO_State). Value is always 1; "+
					"the state string lives in the label so consumers can group / alert "+
					"on transitions away from the steady-state waiting message.",
				host+","+lbl("state", m.IOState), 1)
		}
	}

	// ---- Applications: Custom ----------------------------------------------
	for source, values := range d.Apps.Custom {
		for _, v := range values {
			gauge("kula_custom_metric",
				"User-defined custom metric value.",
				host+","+lbl("source", source)+","+lbl("name", v.Name), v.Value)
		}
	}

	// ---- Kula self-metrics -------------------------------------------------
	gauge("kula_self_cpu_percent",
		"Kula process CPU usage percentage.",
		host, d.Self.CPUPercent)
	gauge("kula_self_memory_rss_bytes",
		"Kula process RSS memory in bytes.",
		host, float64(d.Self.MemRSS))
	gauge("kula_self_open_fds",
		"Number of open file descriptors in the Kula process.",
		host, float64(d.Self.FDs))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// Serves Prometheus text/plain metrics (label values are escaped via escapeLabel); not text/html, so the XSS rule doesn't apply.
	// nosemgrep: no-direct-write-to-responsewriter
	_, _ = w.Write([]byte(b.String()))
}

// formatPrometheusValue keeps exposition output human-readable and stable.
// Whole values (notably byte sizes and counters) stay exact, ordinary
// measurements use at most two decimal places, and very small non-zero rates
// receive enough extra decimal places to avoid being rounded away. Fixed-point
// formatting deliberately prevents strconv from switching to exponent notation.
func formatPrometheusValue(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "+Inf"
	case math.IsInf(value, -1):
		return "-Inf"
	case value == 0:
		return "0"
	case math.Trunc(value) == value:
		return strconv.FormatFloat(value, 'f', 0, 64)
	}

	precision := 2
	if absolute := math.Abs(value); absolute < 0.01 {
		// Retain roughly three significant digits for sub-hundredth rates. The
		// cap is ample for monitoring data while keeping a corrupt/subnormal
		// value from producing hundreds of output characters.
		precision = min(15, max(2, int(math.Ceil(-math.Log10(absolute)))+2))
	}
	formatted := strconv.FormatFloat(value, 'f', precision, 64)
	formatted = strings.TrimRight(strings.TrimRight(formatted, "0"), ".")
	if formatted == "-0" || formatted == "" {
		return "0"
	}
	return formatted
}

// escapeLabel escapes backslashes, double-quotes, and newlines in Prometheus label values.
func escapeLabel(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

// Ensure collector types are referenced (avoids import cycle if moved to its own file).
var _ *collector.Sample
