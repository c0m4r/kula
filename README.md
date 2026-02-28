# 🔮 Kula-Szpiegula

**Lightweight, self-contained Linux® server monitoring tool.**

Zero dependencies. No external databases. Single binary. Just deploy and go.

<img width="1215" height="839" alt="image" src="https://github.com/user-attachments/assets/2a93f22c-74f8-4cf4-8bfb-3cc0f46545e2" />

---

## What It Does

Kula-Szpiegula collects system metrics every second by reading directly from `/proc` and `/sys`, stores them in a built-in tiered ring-buffer storage engine, and serves them through a real-time Web UI dashboard and a terminal TUI.

| Metric | What's Collected |
|--------|-----------------|
| **CPU** | Per-core & total usage (user, system, idle, iowait, irq, steal, guest) |
| **Load** | 1 / 5 / 15 min averages, running & total tasks |
| **Memory** | Total, free, available, used, buffers, cached, shmem, dirty, mapped |
| **Swap** | Total, free, used, cached |
| **Network** | Per-interface throughput (Mbps), packets/s, errors, drops; TCP/UDP/ICMP counters |
| **Disks** | Per-device I/O (read/write bytes/s, utilization %); filesystem & inode usage |
| **System** | Uptime, entropy, clock sync, hostname, logged-in users |
| **Processes** | Task counts by state (running, sleeping, blocked, zombie), total threads |
| **Self** | Kula's own CPU%, RSS/VMS memory, threads, file descriptors |

---

## How It Works

```
                    ┌──────────────────────────────────────────────┐
                    │              Linux Kernel                    │
                    │     /proc/stat  /proc/meminfo  /sys/...      │
                    └──────────────────┬───────────────────────────┘
                                       │ read every 1s
                                       ▼
                              ┌──────────────────┐
                              │    Collectors    │
                              │  (cpu, mem, net, │
                              │   disk, system)  │
                              └────────┬─────────┘
                                       │ Sample struct
                          ┌────────────┼────────────┐
                          ▼            ▼            ▼
                   ┌────────────┐  ┌────────┐  ┌──────────┐
                   │  Storage   │  │  Web   │  │   TUI    │
                   │  Engine    │  │ Server │  │ Terminal │
                   └─────┬──────┘  └───┬────┘  └──────────┘
                         │             │ 
              ┌──────────┼─────────┐   └───────────┐  HTTP + WebSocket
              ▼          ▼         ▼               ▼
          ┌─────────┬─────────┬─────────┐  ┌───────────────┐
          │ Tier 1  │ Tier 2  │ Tier 3  │  │   Dashboard   │
          │   1s    │   1m    │   5m    │  │   (Browser)   │
          │ 250 MB  │ 150 MB  │  50 MB  │  └───────────────┘
          └─────────┴─────────┴─────────┘
             Ring-buffer binary files
             with circular overwrites
```

### Storage Engine

Data is persisted in **pre-allocated ring-buffer files** per tier. Each tier file has a fixed maximum size — when it fills up, new data overwrites the oldest entries. This gives predictable, bounded disk usage with no cleanup needed.

- **Tier 1** — Raw 1-second samples (default 100 MB)
- **Tier 2** — 1-minute aggregates: averaged CPU & network, last-value gauges (default 200 MB)
- **Tier 3** — 5-minute aggregates, same logic (default 200 MB)

### Web Backend

The HTTP server exposes a REST API (`/api/current`, `/api/history`, `/api/config`) and a WebSocket endpoint (`/ws`) for live streaming. Authentication is optional — when enabled, it uses **Whirlpool hashing with salt** and session cookies or Bearer tokens.

### Dashboard SPA

The frontend is a single-page application embedded in the binary. Built on Chart.js with custom SVG gauges, it connects via WebSocket for live updates and falls back to history API for longer time ranges. Features include:

- Interactive zoom with drag-select (auto-pauses live stream)
- Focus mode to show only selected graphs
- Grid / stacked list layout toggle
- Alert system for clock sync & entropy issues
- Gap detection for missing data points

---

## Installation

### Build from Source

```bash
git clone https://github.com/c0m4r/kula.git
cd kula
go build -o kula ./cmd/kula/
```

### Cross-Compile

```bash
bash addons/build.sh cross    # builds amd64, arm64, riscv64
```

### Debian / Ubuntu (.deb)

```bash
bash addons/build_deb.sh
sudo dpkg -i kula_*.deb
```

### Arch Linux (AUR)

```bash
bash addons/build_aur.sh
cd aur && makepkg -si
```

### Docker

```bash
bash addons/docker/build.sh
docker compose -f addons/docker/docker-compose.yml up -d
```

---

## Usage

```
kula [flags] [command]

Commands:
  serve          Start monitoring daemon with web UI (default)
  tui            Launch terminal dashboard
  hash-password  Generate password hash for config

Flags:
  -config string  Path to config file (default "config.yaml")
```

### Quick Start

```bash
# 1. Copy and edit config
cp config.example.yaml config.yaml

# 2. Start the server
./kula serve
# Dashboard at http://localhost:8080

# 3. Or use the terminal UI
./kula tui
```

### Authentication (Optional)

```bash
# Generate password hash
./kula hash-password

# Add the output to config.yaml under web.auth
```

### Service Management

Init system files are provided in `addons/init/`:

```bash
# systemd
sudo cp addons/init/systemd/kula.service /etc/systemd/system/
sudo systemctl enable --now kula

# OpenRC
sudo cp addons/init/openrc/kula /etc/init.d/
sudo rc-update add kula default

# runit
sudo cp -r addons/init/runit/kula /etc/sv/
sudo ln -s /etc/sv/kula /var/service/
```

---

## Configuration

All settings live in `config.yaml`. See [`config.example.yaml`](config.example.yaml) for defaults.

```yaml
collection:
  interval: 1s                   # How often to collect metrics

web:
  enabled: true
  listen: "0.0.0.0"
  port: 8080
  join_metrics: false            # false = show gaps, true = connect lines
  auth:
    enabled: false               # Enable login with Whirlpool-hashed password

storage:
  directory: ./data
  tiers:
    - resolution: 1s
      max_size: 250MB            # Ring-buffer size per tier
    - resolution: 1m
      max_size: 150MB
    - resolution: 5m
      max_size: 50MB
```

---

## Project Structure

```
kula/
├── cmd/kula/
│   └── main.go                 # CLI entry point, flag parsing, commands
├── internal/
│   ├── collector/              # Metric collectors (/proc, /sys readers)
│   │   ├── collector.go        #   Orchestrator — gathers all metrics
│   │   ├── types.go            #   Sample struct (CPU, mem, net, disk...)
│   │   ├── cpu.go              #   /proc/stat parser
│   │   ├── memory.go           #   /proc/meminfo parser
│   │   ├── network.go          #   /proc/net/* parser
│   │   ├── disk.go             #   /proc/diskstats + /proc/mounts
│   │   └── system.go           #   Uptime, entropy, hostname
│   ├── config/                 # YAML config loader with defaults
│   ├── storage/                # Tiered ring-buffer engine
│   │   ├── store.go            #   Multi-tier coordinator + aggregation
│   │   ├── tier.go             #   Single ring-buffer file
│   │   └── codec.go            #   JSON encode/decode for samples
│   ├── tui/                    # Terminal UI (bubbletea + lipgloss)
│   └── web/                    # HTTP/WebSocket server
│       ├── server.go           #   Routes, API handlers, startup
│       ├── websocket.go        #   Live streaming hub + client mgmt
│       ├── auth.go             #   Whirlpool auth, sessions, middleware
│       ├── whirlpool.go        #   Whirlpool hash implementation
│       └── static/             #   Embedded SPA (served from binary)
│           ├── index.html      #     Dashboard markup
│           ├── app.js          #     Charts, WebSocket, UI logic
│           └── style.css       #     Dark theme, glassmorphism
├── addons/
│   ├── build.sh                # Cross-compile (amd64/arm64/riscv64)
│   ├── build_deb.sh            # Debian package builder
│   ├── build_aur.sh            # Arch AUR PKGBUILD generator
│   ├── check.sh                # Linting + testing
│   ├── docker/                 # Dockerfile + compose
│   └── init/                   # Service files (systemd/openrc/runit)
├── docs/
│   ├── kula.1                  # Man page
│   └── kula-completion.bash    # Bash completion
├── config.example.yaml
├── VERSION                     # Single source of truth for version
├── CHANGELOG
└── LICENSE                     # AGPL-3.0
```

---

## Development

```bash
# Run tests
go test -v -race ./...

# Lint + test suite
bash addons/check.sh

# Build
go build -o kula ./cmd/kula/
```

Binary size: ~11 MB (static, CGO_ENABLED=0)

---

## License

[GNU Affero General Public License v3.0](LICENSE)

Linux® is the registered trademark of Linus Torvalds in the U.S. and other countries.
