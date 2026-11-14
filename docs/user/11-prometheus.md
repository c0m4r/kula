# Prometheus Exporter

Kula can expose all its metrics in Prometheus text exposition format at `/metrics`, so you can
scrape it into an existing observability stack alongside the built-in dashboard.

## Enable it

```yaml
web:
  prometheus_metrics:
    enabled: true
    token: ""   # optional bearer token
```

The endpoint is then available at:

```
http://localhost:27960/metrics
```

It works even with `web.ui: false` — you can run Kula purely as an exporter with no dashboard.

## Authentication (optional)

By default `/metrics` is unauthenticated, and answers only local `Host` names (see below). Set
a bearer token to protect it:

```yaml
  prometheus_metrics:
    enabled: true
    token: "a-long-random-secret"
```

Scrapers must then send:

```
Authorization: Bearer a-long-random-secret
```

The scheme name may be written in any case (`bearer` works too); the token must match exactly,
and the comparison is constant-time. In Prometheus:

```yaml
scrape_configs:
  - job_name: kula
    authorization:
      type: Bearer
      credentials: "a-long-random-secret"
    static_configs:
      - targets: ["server:27960"]
```

Without a token, `/metrics` answers only local `Host` names, whether or not web auth is enabled
(DNS rebinding protection, see [`web.security.allowed_hosts`](04-configuration.md#websecurity)):
a web page cannot read it by pointing its own domain at your server. A scrape target that is a
regular DNS name, such as `kula.example.com:27960`, therefore gets `403` until the name is
listed in `allowed_hosts`. With a bearer token set, `/metrics` answers any `Host`.

## Exposed metrics

All metrics are prefixed `kula_`. The exporter covers the full collection surface:

**CPU / load / processes:** `kula_cpu_usage_percent`,
`kula_cpu_{user,system,iowait,irq,softirq,steal}_percent`, `kula_cpu_cores`,
`kula_cpu_temperature_celsius`, `kula_cpu_sensor_temperature_celsius`, `kula_load_average_*`,
`kula_processes_running`, `kula_processes_sleeping`, `kula_processes_blocked`,
`kula_processes_zombie`, `kula_processes_total`, `kula_threads_total`.

**Memory / swap:** `kula_memory_total_bytes`, `kula_memory_used_bytes`,
`kula_memory_free_bytes`, `kula_memory_available_bytes`, `kula_memory_buffers_bytes`,
`kula_memory_cached_bytes`, `kula_memory_shmem_bytes`, `kula_memory_used_percent`, and the
`kula_swap_*` equivalents.

**Network / TCP / sockets:** `kula_network_rx_mbps`, `kula_network_tx_mbps`,
`kula_network_{rx,tx}_packets_per_second`, `kula_network_{rx,tx}_{bytes,packets,errors,drops}_total`,
`kula_tcp_established`, `kula_tcp_errors_per_second`, `kula_tcp_resets_per_second`,
`kula_tcp_retransmissions_per_second`, `kula_sockets_tcp_in_use`, `kula_sockets_tcp_time_wait`, `kula_sockets_udp_in_use`.

**Disk / filesystem:** `kula_disk_info`, `kula_disk_reads_per_second`,
`kula_disk_writes_per_second`, `kula_disk_read_bytes_per_second`,
`kula_disk_write_bytes_per_second`, `kula_disk_utilization_percent`,
`kula_disk_temperature_celsius`, `kula_filesystem_size_bytes`,
`kula_filesystem_used_bytes`, `kula_filesystem_available_bytes`,
`kula_filesystem_used_percent`.

**System:** `kula_system_uptime_seconds`, `kula_system_entropy_available`,
`kula_system_clock_synced`, `kula_system_logged_in_users` (counted from utmp, or from
systemd-logind sessions where utmp is missing or records no login; omitted when the host has
neither, and in a container that does not mount them, see
[Docker](02-installation.md#docker)).

**GPU:** `kula_gpu_temperature_celsius`, `kula_gpu_load_percent`, `kula_gpu_power_watts`,
`kula_gpu_vram_total_bytes`, `kula_gpu_vram_used_bytes`, `kula_gpu_vram_used_percent`.

**Power supply / battery:** `kula_psu_capacity_percent`, `kula_psu_voltage_volts`,
`kula_psu_current_amperes`, `kula_psu_power_watts`, `kula_psu_energy_now_wh`,
`kula_psu_energy_full_wh`.

**Self (Kula's own usage):** `kula_self_cpu_percent`, `kula_self_memory_rss_bytes`,
`kula_self_open_fds`.

**Applications (when enabled):**

- **nginx:** `kula_nginx_active_connections`, `kula_nginx_{reading,writing,waiting}`,
  `kula_nginx_{accepts,handled,requests}_per_second`, `kula_nginx_{accepts,handled,requests}_total`.
- **Apache2:** `kula_apache2_busy_workers`, `kula_apache2_idle_workers`,
  `kula_apache2_open_slots`, `kula_apache2_accesses_per_second`,
  `kula_apache2_served_bytes_per_second`, `kula_apache2_bytes_per_request`,
  `kula_apache2_cpu_load`, `kula_apache2_accesses_total`, `kula_apache2_kbytes_total`,
  `kula_apache2_scoreboard`, `kula_apache2_uptime_seconds`. The two rates are current (the
  change since the previous poll; they need `ExtendedStatus On`); `bytes_per_request` and
  `cpu_load` are Apache's averages since it started.
  **Deprecated:** `kula_apache2_requests_per_second` and `kula_apache2_bytes_per_second` are
  also averages since Apache started, not current rates, though their names say otherwise.
  They stay for now so existing queries keep working, and will be removed in a later release;
  use `kula_apache2_accesses_per_second` and `kula_apache2_served_bytes_per_second` instead.
- **PostgreSQL:** `kula_postgres_connections_*`, `kula_postgres_transactions_*_per_second`,
  `kula_postgres_tuples_*_per_second`, `kula_postgres_buffer_cache_hit_percent`,
  `kula_postgres_blocks_*_per_second`, `kula_postgres_buffers_*_per_second`,
  `kula_postgres_deadlocks_per_second`,
  `kula_postgres_{dead,live}_tuples`, `kula_postgres_autovacuum_count`,
  `kula_postgres_database_size_bytes`, `kula_postgres_replication_lag_{bytes,seconds}`,
  `kula_postgres_replicas_connected`, `kula_postgres_is_in_recovery`.
- **MySQL / MariaDB:** `kula_mysql_threads_{connected,running,cached}`,
  `kula_mysql_max_connections`,
  `kula_mysql_{queries,select,insert,update,delete,slow_queries}_per_second`,
  `kula_mysql_innodb_buffer_pool_reads_per_second`,
  `kula_mysql_innodb_buffer_pool_hit_percent`, `kula_mysql_row_lock_waits_per_second`,
  `kula_mysql_table_locks_waited_per_second`, `kula_mysql_replicas_connected`, and the
  `kula_mysql_replica_*` state series.
- **Containers:** `kula_container_cpu_percent`, `kula_container_memory_{used,limit}_bytes`,
  `kula_container_memory_used_percent`, `kula_container_network_{rx,tx}_bytes_per_second`,
  `kula_container_disk_{read,write}_bytes_per_second`.
- **Custom metrics:** `kula_custom_metric{source="...",name="..."}` — one series per
  user-defined metric received on the custom-metrics socket (see
  [Custom Metrics](09-custom-metrics.md)).

Per-device metrics (network interfaces, disks, filesystems, GPUs, containers, sensors) carry
appropriate labels. Each metric family has one `# HELP` and one `# TYPE` line followed by all of
its series, so strict parsers such as `promtool check metrics` accept the output. Label values
are valid UTF-8 (bytes that are not become `�`), and names longer than 255 bytes are shortened
as in history (see [Containers](08-application-monitoring.md#containers-docker--podman)). The
container series are bounded by the collector's cap of 1,024 containers.

Disk temperatures and power-supply metrics change every `collection.disk_temp_interval` and
`collection.power_supply_interval` (10 s by default), PostgreSQL's dead/live tuples,
autovacuum count and database size once a minute; a scrape in between returns the last
reading.

Disk I/O and temperature metrics use the **persistent disk ID** as their `device` label value,
so a drive's history follows it across reboots and kernel renames. `kula_disk_info` maps that
ID to the drive's current kernel name:
`kula_disk_info{device="...",kernel_name="sda",identity_source="wwid"} 1`. Disks without a
usable identity fall back to `device="kernel:<name>"` and `identity_source="kernel"`. Filesystem
metrics are unaffected — their `device` label remains the mount source (for example
`/dev/sda1`). Upgrading to 0.20.0 starts new disk series under these label values, so update
dashboards and alert rules that filter on old kernel-name values (for example `device="sda"`).

> This list reflects version `0.22.0`. For the authoritative, always-current set, scrape your
> instance and inspect the output, or see the wiki
> [Prometheus metrics page](https://github.com/c0m4r/kula/wiki/Prometheus-metrics).

Next: [Backups](12-backups.md).
