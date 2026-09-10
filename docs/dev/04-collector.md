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
| [`network.go`](../../internal/collector/network.go) | `/proc/net/dev`, `/proc/net/snmp`, `/proc/net/netstat`, `/proc/net/sockstat` | per-iface throughput, TCP errors/resets/retrans/established, sockets |
| [`disk.go`](../../internal/collector/disk.go) | `/proc/diskstats`, `statfs`, hwmon | per-device I/O, filesystem usage, disk temps (skips virtual/LVM/loop) |
| [`disk_identity.go`](../../internal/collector/disk_identity.go) | sysfs (`wwid`, `serial`, `uuid`), `kula disks` | persistent disk IDs and stable per-disk series keys |
| [`system.go`](../../internal/collector/system.go) | hostname, `/proc/uptime`, entropy, `adjtimex`, utmp | uptime, entropy, clock sync, user count |
| [`process.go`](../../internal/collector/process.go) | `/proc/<pid>/stat` | running/sleeping/blocked/zombie counts, threads |
| [`self.go`](../../internal/collector/self.go) | `/proc/self/*` | Kula's own CPU%, RSS, open FDs |
| [`gpu.go`](../../internal/collector/gpu.go) | `/sys/class/drm` discovery | GPU enumeration (NVIDIA/AMD/Intel) |
| [`gpu_nvidia.go`](../../internal/collector/gpu_nvidia.go) | `nvidia.log` (CSV, atomic read) | NVIDIA temp/load/power/VRAM |
| [`gpu_sysfs.go`](../../internal/collector/gpu_sysfs.go) | sysfs | AMD/Intel temp/power/VRAM/load |
| [`psu.go`](../../internal/collector/psu.go) | `/sys/class/power_supply` | battery/PSU status |
| [`containers.go`](../../internal/collector/containers.go) | Docker/Podman socket + cgroups v2 | per-container CPU/mem/net/disk |
| [`nginx.go`](../../internal/collector/nginx.go) | stub_status | connections, accepts/handled/requests rates |
| [`apache2.go`](../../internal/collector/apache2.go) | mod_status | workers, scoreboard, rates |
| [`postgres.go`](../../internal/collector/postgres.go) | `lib/pq`, `pg_stat_*` | connections, txns, tuples, I/O, locks, size, replication |
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
  `nginx.go` and `apache2.go`).
- **Lazy-allocate clients.** HTTP/DB clients are created on first use.
- **Debug logging** goes through the `debugf` helper, gated by `collection.DebugLog` (which is
  set when `web.logging.level == "debug"`).

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
