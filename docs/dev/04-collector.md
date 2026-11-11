# Collector Subsystem

Package: [`internal/collector`](../../internal/collector/)

The collector reads the kernel's `/proc` and `/sys` filesystems (and optional application
endpoints) and produces one `Sample` per tick. It is the single source of all metrics.

## Orchestration

[`collector.go`](../../internal/collector/collector.go) defines the `Collector` struct and
`Collect()` method. `Collect()` calls each sub-collector, assembles a `Sample`, and computes
per-second rates from the previous reading. The main loop in `cmd/kula/main.go` calls
`Collect()` every `collection.interval`.

Lifecycle:

- `collector.New(global, collection, applications, storageDir)` — construct.
- `coll.StartApplications()` — initialize optional app collectors (DB connections, HTTP
  clients, the custom-metrics socket) after the startup banner.
- `coll.Collect()` — produce a `Sample`.
- `coll.Stop()` — clean shutdown.

## Sub-collectors

| File | Source | Produces |
|------|--------|----------|
| [`cpu.go`](../../internal/collector/cpu.go) | `/proc/stat`, `/proc/loadavg`, hwmon/thermal sysfs | CPU usage breakdown, load averages, CPU temps |
| [`memory*`](../../internal/collector/) (in `cpu.go`/types) | `/proc/meminfo` | memory + swap |
| [`network.go`](../../internal/collector/network.go) | `/proc/net/dev`, `/proc/net/snmp`, `/proc/net/sockstat` | per-iface throughput, TCP errors/resets/retrans/established, sockets |
| [`disk.go`](../../internal/collector/disk.go) | `/proc/diskstats`, `statfs` ([`mount_stats.go`](../../internal/collector/mount_stats.go)), hwmon ([`cadence.go`](../../internal/collector/cadence.go)) | per-device I/O and busy % (`io_ticks`), filesystem usage, disk temps every `disk_temp_interval` (skips virtual/LVM/loop) |
| [`disk_identity.go`](../../internal/collector/disk_identity.go) | sysfs (`wwid`, `serial`, `uuid`), `kula disks` | persistent disk IDs and stable per-disk series keys |
| [`system.go`](../../internal/collector/system.go) | hostname, `/proc/uptime`, entropy, `adjtimex`, utmp or systemd-logind sessions | uptime, entropy, clock sync, user count (`-1` when neither source exists) |
| [`process.go`](../../internal/collector/process.go) | `/proc/<pid>/stat` | running/sleeping/blocked/zombie counts, threads |
| [`self.go`](../../internal/collector/self.go) | `/proc/self/*` | Kula's own CPU%, RSS, open FDs |
| [`gpu.go`](../../internal/collector/gpu.go) | `/sys/class/drm` discovery | GPU enumeration (NVIDIA/AMD/Intel); GPUs found are kept, a host without one looks again once a minute |
| [`gpu_nvidia.go`](../../internal/collector/gpu_nvidia.go) | `nvidia.log` (CSV, atomic read) | NVIDIA temp/load/power/VRAM |
| [`gpu_sysfs.go`](../../internal/collector/gpu_sysfs.go) | sysfs | AMD/Intel temp/power/VRAM/load |
| [`psu.go`](../../internal/collector/psu.go) | `/sys/class/power_supply` ([`cadence.go`](../../internal/collector/cadence.go)) | battery/PSU status every `power_supply_interval` |
| [`containers.go`](../../internal/collector/containers.go) | Docker/Podman socket + cgroups v2 | per-container CPU/mem/net/disk; a container is inspected for its PID once, kept while `/proc/<pid>/cgroup` still names it, and its state dropped when it leaves the list; the list is stream-decoded up to 64 MiB (about 25,000 containers), and a failure to read it is logged when it changes. See [Containers](#containers) |
| [`nginx.go`](../../internal/collector/nginx.go) | stub_status | connections, accepts/handled/requests rates |
| [`apache2.go`](../../internal/collector/apache2.go) | mod_status | workers, scoreboard, rates |
| [`postgres.go`](../../internal/collector/postgres.go) | `lib/pq`, `pg_stat_*` | connections, txns, tuples, I/O, locks, size, replication; table health and size once a minute |
| [`mysql.go`](../../internal/collector/mysql.go) | `go-sql-driver/mysql`, `SHOW GLOBAL STATUS` | threads, queries, InnoDB, replication |
| [`custom.go`](../../internal/collector/custom.go) | Unix socket `kula.sock` (JSON) | user-defined chart groups |
| [`ai.go`](../../internal/collector/ai.go) | the current `Sample` | `FormatForAI()` text for the LLM |
| [`util.go`](../../internal/collector/util.go) | — | safe `parseUint/parseInt/parseFloat` wrappers |

## Data types

All metric structs live in [`types.go`](../../internal/collector/types.go). The top-level
`Sample` aggregates: `CPUStats`, `LoadAvg`, `MemoryStats`, `SwapStats`, `NetworkStats`,
`DiskStats`, `SystemStats`, `GPUStats`, `ContainerStats`, `PostgresStats`, `MysqlStats`,
`Apache2Stats`, `NginxStats`, `PowerSupplyStats`, process/self stats, and an
`ApplicationsStats` holder for the optional modules. Every struct carries JSON tags for the
API and WebSocket payloads.

## Conventions for sub-collectors

- **Return `nil` on failure** rather than erroring out — a missing subsystem must not break the
  whole sample. The encoder treats `nil` sections as absent.
- **Guard cumulative counters against resets.** Sources like nginx/apache2 reset their counters
  on restart; the delta computation guards against negative deltas to avoid absurd rates (see
  `nginx.go` and `apache2.go`). Guard *every* counter, not one representative: nginx reports no
  rates for a poll in which any of its counters went backwards, since they reset together.
- **Rates span consecutive successful polls only.** A failed poll drops the baseline (and a
  failed sub-query drops that counter's), so the next delta is never two intervals of growth
  divided by one (`nginx.go`, `apache2.go`, `postgres.go`, `mysql.go`). A counter missing from a
  poll (Apache2 without `ExtendedStatus`) is no baseline either.
- **Names fit in storage.** `Collect()` bounds every name and label in the sample to the 255
  bytes the codec keeps (`BoundString`, [`bound.go`](../../internal/collector/bound.go)): a
  longer one keeps its start, cut on a rune boundary, and ends in `…` and eight hex digits of a
  hash of the whole string, so names that share a prefix stay distinct identities. Storage,
  the live views, `/metrics` and the TUI all see the bounded name. A slice a subsystem hands to
  several samples is copied before anything in it changes. One over-long name (a long mount
  path, a container named by whoever ran it) used to fail the whole record and stop history
  recording.
- **Lazy-allocate clients.** HTTP/DB clients are created on first use.
- **`statfs` runs off the collection tick.** On a hard NFS mount whose server is gone (or a
  stalled FUSE daemon) it blocks in the kernel, beyond any timeout. `mount_stats.go` keeps at
  most one call in flight per mount point, waits `statfsWait` (at most 200 ms) for a tick's
  calls, and reports a mount whose call is still running with its last result while that is
  under `pollStale`, then not at all, logging when it stops and starts answering.
- **Readings that cost the device run at their own interval.** Disk temperatures and power
  supplies go through [`cadence.go`](../../internal/collector/cadence.go): each NVMe
  temperature read sends the drive a SMART command (2–5 ms per sensor, and it can keep the
  drive out of its deeper idle states), and a laptop battery read is an ACPI round trip to the
  embedded controller (about 80 ms on the host where this was measured, paid whenever the
  kernel's one-second battery cache has expired). `collection.disk_temp_interval` and `collection.power_supply_interval`
  (default 10 s, a multiple of the collection interval, resolved by
  `config.ReadingInterval`) set how often; the samples in between repeat the last reading,
  each with its own copy, so the record format and aggregation are unchanged. Each key (a
  disk's kernel name, checked against its identity and `diskseq`; the power supply class) has
  at most one read in flight, in its own goroutine. The tick that starts a read waits
  `statfsWait` for it; a read is due up to half a tick early, so jitter cannot push it a tick
  late; a reading stays current for `pollStale` of its interval; and a read outstanding for a
  whole interval with nothing current to show is logged, once each way. A disk swapped in
  under a known name is read at once, and only disks selected for monitoring are read.
  `mountStats` is the same arrangement for `statfs`, which runs every tick. The System Info
  page (`internal/sysinfo`) reads its sensors and power supplies no more often than these
  intervals either (`Collector.ReadingIntervals`), and not at all while
  `global.show_system_details` is off.
- **Application polls run off the collection tick.** `poller.go`'s `poller[T]` runs one poll at
  a time in its own goroutine, and `Collect` reads `Latest()`, which is `nil` once a result is
  older than `pollStale` (three intervals, at least 3 s). nginx and Apache2 use it through
  `startStatusPoller` (request timeout `max(2s, 2×interval)`, no redirects, failures logged on
  change by `pollLog`); their clients, baselines and logs belong to the poll goroutine, so
  `debugDone`, which `Collect` writes, is atomic.
- **A poll that panics fails alone.** lib/pq and go-sql-driver/mysql index server packets
  without checking every length, so one malformed reply — from a broken server, or from
  whoever answers on its port — panics inside the driver, and in a goroutine an unrecovered
  panic ends the process. `poller.run` recovers it: the poll yields `nil`, the panic is logged
  once per streak (escaped, with the frame it started in), and the poller's `discard` drops
  what the poll was in the middle of — a status page's rate baseline, or a database session
  (`dbSession.discard`). database/sql releases a connection's lock only when its statement
  returns, so after a panic mid-statement `Conn.Close` would block forever; `discard` instead
  shuts the dialer (`dbDialer.shut`: closes its connections and refuses new ones until the
  next attempt's `reopen`, because lib/pq answers the end of the interrupted query's context by
  dialing the server to cancel it, from a goroutine that reads that connection until the
  server closes it), closes the driver connection through `Conn.Raw`, closes the pool,
  remembers the session for reaping and backs off. Every `bound` is disarmed by a deferred
  cancel, so a panic leaves no abort timer armed against the next session. `cleanup` runs
  under the same guard. `poll_panic_test.go` drives both real drivers against the malformed
  replies and checks that nothing stays open. One leftover cannot be reached from outside
  the driver: a panic during go-sql-driver/mysql's connection phase leaves that connection's
  watcher goroutine parked, about 13 KB each, one per attempt under the backoff (at most 288
  a day once it reaches 5 minutes).
- **Database collectors own exactly one session.** `postgres.go` and `mysql.go` poll through
  `dbsession.go`: a pinned `sql.Conn` from a pool capped at one; polls in their own goroutine,
  one at a time (`poller`, read by `Collect` through `Latest()`), with a hard timeout of
  `max(10s, 2×interval)`, at most one minute, instead of the collection interval; server-side statement, lock, idle
  and keepalive limits set on every session; a session given up on is ended by the next
  connection (matched by PostgreSQL pid + `backend_start`, MySQL connection id + user + server
  start) before polling resumes, and the collector closes its probe connection and backs off
  while it lives; failed attempts back off exponentially from one interval to 5 minutes.
  lib/pq keeps reading after a context ends, so both drivers dial through `dbDialer`, which
  uses short TCP keepalives for an idle half-open connection and tracks its connections so
  that `dbSession.bound` can close them when work outlives its deadline by `abortGrace` (5 s):
  keepalives cannot detect a frozen server whose kernel still acknowledges, or data waiting on
  the retransmission timer. `dbsession_test.go` checks the bounds (one client connection, at
  most two server sessions) against a fake driver.
- **PostgreSQL's relation statistics are read once a minute.** The `pg_stat_user_tables` sums
  and `pg_database_size` walk every relation, the latter with a `stat()` of each file (about
  30 and 60 ms of server time at 10 000 tables), so `postgres.go` reads them every
  `pgRelationStatsInterval` and repeats them in the polls between. Every attempt counts,
  failed or not, so statistics slower than the statement timeout are not retried each poll.
  A failed query keeps the previous read's values until the next read; a second failure in a
  row reports zeros.
  `postgres_test.go` counts the queries of two minutes of polls against a fake server.
- **MySQL TLS uses PostgreSQL's names.** `sslmode` (`disable`, `require`, `verify-ca`,
  `verify-full`) and `sslrootcert` behave as libpq's, `require` with a CA included.
  `mysql_tls.go` builds the `tls.Config` in the driver's `BeforeConnect` hook, so the CA file
  is read for each connection; `verify-ca` checks the chain in `VerifyConnection`, because
  crypto/tls verifies a chain only together with the name. Config loading refuses the settings
  that would read as safer than they are (TLS on a Unix socket, `verify-ca` without a CA,
  `verify-full` without a host). `mysql_tls_test.go` runs every mode against a fake server
  speaking the MySQL handshake and TLS.
- **Debug logging** goes through the `debugf` helper, gated by `collection.DebugLog` (which is
  set when `web.logging.level == "debug"`).
- **Text a monitored service chose is logged escaped.** A server's error message, a status
  page or a name in its certificate can hold line breaks and terminal escapes, so `debugf`,
  `pollLog` and `dbSession` pass every line through `logSafe` (control characters, invisible
  formatting characters and invalid UTF-8 become Go escapes), and a status URL is logged
  through `redactURL`/`redactURLError`, without its `user:password@`. MySQL's replica IO
  state, which a sample stores, is kept valid UTF-8 and cut on a character boundary
  (`truncateUTF8`). `untrusted_text_test.go` covers each.

## Containers

A container runtime, and in cgroups mode whoever can create a cgroup, chooses what this
collector reads, and the TUI often runs it as root. So:

- **Every ID is a full container ID**, 64 lowercase hex digits (`isContainerID`), whether the
  runtime listed it or a cgroup directory is named after it. IDs become part of the inspect URL
  and of cgroup paths, so none can carry a path, control characters or markup, and two
  containers cannot share a 12-character short ID. Running containers listed with another ID
  are skipped, and their number is logged when it changes.
- **Auto-detection tries only Kula's own rootless socket.** Another user's rootless Podman
  would choose the containers, IDs and PIDs a root Kula reads; it takes an explicit
  `socket_path`. `ContainerSocketPaths` is the one list the collector and the sandbox share.
- **Cgroups mode counts only cgroups a runtime made.** systemd delegates
  `user.slice/user-<uid>.slice/user@<uid>.service` to each logged-in user (and a service's
  subtree to its user with `Delegate=yes`), who can create any directory there. A directory
  counts only when it is named after a full ID, is a directory rather than a link, and is owned
  by root or by Kula's own user (`runtimeOwned`); the rootful Quadlet patterns skip
  `user.slice`, and the rootless one searches Kula's own user manager only. The socket-mode
  fallback (`findCgroupDir`) applies the same rule.
- **At most `maxContainers` (1,024) per sample.** Each adds about 66 bytes to every stored
  record. Past the cap, the containers whose names (IDs in cgroups mode) sort first are kept, so
  the set stays stable, and the cut is logged when it changes.
- **Files are read bounded** (`readBoundedFile`): at most 1 MiB, without following a final
  symbolic link or waiting on a FIFO, and only from a regular file.

## Auto-discovery overrides

Disks, network interfaces, and mount points are auto-discovered, but the user can pin them via
`collection.devices`, `collection.interfaces`, and `collection.mountpoints`. Mount detection
mode (`auto`/`host`/`self`) controls which `/proc/.../mounts` file is read.

## Hardware-limit detection

For "auto" chart bounds, the collector exposes detectors used by `/api/config`:
`DetectTjMax()` (CPU thermal limit), `DetectDiskTjMax()` (disk thermal limit), and
`DetectLinkSpeed()` (NIC link speed).

## Adding a new application collector

Adding a metric type touches the collector **and** the storage codec, config, sandbox,
frontend, and Python decoder. The full checklist is in
[Adding a Metric Type](14-adding-metrics.md).

## Tests

Collectors are tested against fixture trees under
[`internal/collector/testdata/`](../../internal/collector/testdata/) (synthetic `/proc` and
`/sys` layouts), plus fuzz targets (`parse_fuzz_test.go`, `process_fuzz_test.go`) and
benchmarks (`bench_test.go`). See [Testing & QA](12-testing.md).

Next: [Storage Engine](05-storage-engine.md).
