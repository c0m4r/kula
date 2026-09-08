# Introduction

Kula is a **lightweight, self-contained Linux server monitoring tool**. It is a single Go
binary with no runtime dependencies, no external database, and no cloud connection. You
upload it to a server, run it, and immediately get real-time metrics through a web dashboard,
a terminal UI, and a Prometheus endpoint.

## Design goals

- **Self-contained** — one statically-linked binary embeds the web UI, fonts, locales, and
  storage engine. Nothing else to install.
- **Lightweight** — reads directly from the kernel's `/proc` and `/sys` interfaces; the
  production binary is ~14 MB (~4 MB compressed).
- **Private** — no telemetry, no ads, no registration, no third-party APIs. Works fully in
  air-gapped networks.
- **Secure by default** — optional Argon2id authentication, CSRF/CSP/HSTS protections, and a
  Linux Landlock sandbox that confines Kula's filesystem and network access at runtime.

## What it collects

Kula reads metrics every second (configurable) and tracks:

| Category | What's collected |
|----------|------------------|
| **CPU** | Total usage broken down into user, system, iowait, irq, softirq, steal; core count |
| **Load** | 1 / 5 / 15-minute averages; running & total tasks |
| **Memory** | Total, free, available, used, buffers, cached, shmem |
| **Swap** | Total, free, used |
| **Network** | Per-interface throughput (Mbps), packets/s, errors, drops; TCP errors/s, resets/s, retransmits, established connections; socket counts |
| **Disks** | Per-device I/O (read/write bytes/s, IOPS), utilization; filesystem usage |
| **Thermal** | CPU, GPU, and disk temperatures |
| **GPU** | Load, power, VRAM, temperature (NVIDIA, AMD, Intel) |
| **Battery / PSU** | Power-supply and battery status from `/sys/class/power_supply` |
| **System** | Uptime, available entropy, clock sync, hostname, logged-in user count |
| **Processes** | Running, sleeping, blocked, zombie counts; thread count |
| **Self** | Kula's own CPU%, RSS memory, open file descriptors |
| **Containers** | Docker / Podman / raw cgroups |
| **Applications** | PostgreSQL, MySQL/MariaDB, nginx, Apache2 |
| **Custom** | Anything you feed through the custom-metrics Unix socket |

## How it works

```
    ╭──────────────────────────────────────────────╮
    │                  Linux Kernel                │
    │      /proc/stat  /proc/meminfo  /sys/...     │
    ╰───────────────────────┬──────────────────────╯
                            │ Read every 1s
                            ▼
    ╭──────────────────────────────────────────────╮
    │                   Collectors                 │
    │        (CPU, Mem, Net, Disk, System, ...)    │
    ╰───────────────────────┬──────────────────────╯
                            │ Live Sample
         ╭──────────────────┼─────────────────────╮
         ▼                  ▼                     ▼
╭─────────────────╮  ╭────────────────╮  ╭─────────────────╮
│ Storage Engine  │  │   Web Server   │  │   TUI Terminal  │
╰───┬─────────┬───╯  ╰──────┬─────────╯  ╰─────────────────╯
    │         │             │
    │         ╰──(History)──┤              ╭───────────────╮
    │                       ╰──(HTTP/WS)─► │   Dashboard   │
    ▼                                      ╰───────────────╯
╭──────────┬──────────┬──────────╮
│  Tier 0  │  Tier 1  │  Tier 2  │
│    1s    │    1m    │    5m    │
│  250 MB  │  150 MB  │  50 MB   │
╰──────────┴──────────┴──────────╯
 Ring-buffer binary files
```

Each second the **collectors** produce a single `Sample` containing every metric. That sample
is simultaneously:

1. **Written to the storage engine** — a tiered ring-buffer that keeps raw 1-second data in
   Tier 0, and progressively downsamples to 1-minute (Tier 1) and 5-minute (Tier 2)
   aggregates as data ages. Each tier is a fixed-size binary file that wraps around and
   overwrites its oldest entries.
2. **Broadcast to connected web clients** over WebSocket for live charts.

The web dashboard pulls live data over WebSocket and falls back to the history REST API for
longer time ranges. The TUI is a separate, self-contained terminal view that collects its own
samples.

See the [Architecture Overview](../dev/01-architecture.md) for the developer-level picture.

## Platform support

- **OS:** Linux only (relies on the Linux `/proc` and `/sys` filesystems).
- **Architectures:** amd64 (x86_64), arm64, riscv64 (release packages provided).
- **Kernel:** the Landlock sandbox requires kernel **5.13+**, but Kula runs (without sandbox
  enforcement) on older kernels via best-effort degradation.

## License

Kula is released under the [GNU Affero General Public License v3.0](../../LICENSE).

Continue to [Installation](02-installation.md).
