<div align="center">

<img width="128" height="128" alt="image" src="https://github.com/user-attachments/assets/77d5850a-c3a4-47fe-b33e-dcaeeb3c8e4d" />

# K U L A

**Lightweight, self-contained Linux® server monitoring tool.**

![Go](https://img.shields.io/badge/made%20for-linux-yellow?logo=linux&logoColor=ffffff)
![Go](https://img.shields.io/badge/go%20go-power%20rangers-blue?logo=go&logoColor=ffffff)
![JS](https://img.shields.io/badge/some%20-js-orange?logo=javascript&logoColor=ffffff)
![JS](https://img.shields.io/badge/and%20a%20pinch%20of-bash-green?logo=linux&logoColor=ffffff)
[![License: GPL v3](https://img.shields.io/badge/License-AGPLv3-red.svg)](https://www.gnu.org/licenses/agpl-3.0)

Zero dependencies. No external databases. Single binary. Just deploy and go.

<img width="1037" height="793" alt="image" src="https://github.com/user-attachments/assets/d50e5653-0e84-4374-9ded-4d8e9f8eedd2" />

</div>

---

## What It Does

Kula collects system metrics every second by reading directly from `/proc` and `/sys`, stores them in a built-in tiered ring-buffer storage engine, and serves them through a real-time Web UI dashboard and a terminal TUI.

| Metric | What's Collected |
|--------|-----------------|
| **CPU** | Total usage (user, system, iowait, irq, softirq, steal) + core count |
| **Load** | 1 / 5 / 15 min averages, running & total tasks |
| **Memory** | Total, free, available, used, buffers, cached, shmem |
| **Swap** | Total, free, used |
| **Network** | Per-interface throughput (Mbps), packets/s, errors, drops; TCP errors/s, resets/s, established connections; socket counts |
| **Disks** | Per-device I/O (read/write bytes/s, reads/s, writes/s IOPS); filesystem usage |
| **System** | Uptime, entropy, clock sync, hostname, logged-in user count |
| **Processes** | Running, sleeping, blocked, zombie counts |
| **Self** | Kula's own CPU%, RSS memory, open file descriptors |

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

- **Tier 1** — Raw 1-second samples (default 250 MB)
- **Tier 2** — 1-minute aggregates: averaged CPU & network, last-value gauges (default 150 MB)
- **Tier 3** — 5-minute aggregates, same logic (default 50 MB)

### Web Backend

The HTTP server exposes a REST API (`/api/current`, `/api/history`, `/api/config`) and a WebSocket endpoint (`/ws`) for live streaming. Authentication is optional — when enabled, it uses **Argon2id hashing with salt** and session cookies or Bearer tokens.

### Dashboard SPA

The frontend is a single-page application embedded in the binary. Built on Chart.js with custom SVG gauges, it connects via WebSocket for live updates and falls back to history API for longer time ranges. Features include:

- Interactive zoom with drag-select (auto-pauses live stream)
- Focus mode to show only selected graphs
- Grid / stacked list layout toggle
- Alert system for clock sync & entropy issues
- Gap detection for missing data points

---

## Installation

Example installation methods for **amd64 (x86_64)** GNU/Linux.

Check [Releases](https://github.com/c0m4r/kula/releases) for **ARM** and **RISC-V** packages.

### Standalone

```bash
wget https://github.com/c0m4r/kula/releases/download/0.7.1/kula-0.7.1-amd64.tar.gz
echo "6baff6bee9f9bbf56adc6e264e7ff9e1dfa763e7bab76a21dbc1e7d4be0397f4 kula-0.7.1-amd64.tar.gz" | sha256sum -c || rm kula-0.7.1-amd64.tar.gz
tar -xvf kula-0.7.1-amd64.tar.gz
cd kula
./kula
```

### Debian/Ubuntu

```bash
wget https://github.com/c0m4r/kula/releases/download/0.7.1/kula_0.7.1_amd64.deb
echo "bbcd6ee65441c85f5bc835c40a1afaabc78b78d976c25e535c051b29ad514185 kula_0.7.1_amd64.deb" | sha256sum -c || rm kula_0.7.1_amd64.deb
sudo dpkg -i kula_0.7.1_amd64.deb
systemctl status kula
```

### Build from Source

```bash
git clone https://github.com/c0m4r/kula.git
cd kula
bash addons/build.sh
```

---

## Usage

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

### Running behind reverse proxy (nginx)

```nginx
server {
    listen 80 ;
    listen [::]:80 ;
    server_name kula.localhost;

    location / {
        proxy_pass http://localhost:8080;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

---

## Configuration

All settings live in `config.yaml`. See [`config.example.yaml`](config.example.yaml) for defaults.

---

## Development

```bash
# Lint + test suite
bash ./addons/check.sh

# Build dev (Binary size: ~11MB)
CGO_ENABLED=0 go build -o kula ./cmd/kula/

# Build prod (Binary size: ~8MB)
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -buildvcs=false -o kula ./cmd/kula/
```

### Updating Dependencies

To safely update only the Go modules used by Kula to their latest minor/patch versions, and prune any unused dependencies:

```bash
go get -u ./...
go mod tidy
```

### Testing & Benchmarks

```bash
# Run unit tests with race detector
go test -race ./...

# Run the full storage benchmark suite (default: 3s per bench)
bash addons/benchmark.sh

# Shorter run for quick iteration
bash addons/benchmark.sh 500ms

# Python scripts formatter and linters
black addons/*.py
pylint addons/*.py
mypy --strict addons/*.py
```

Benchmarks cover the full storage engine: codec encode/decode, ring-buffer write throughput, concurrent writes, QueryRange at various sizes (small/large/wrapped), the `QueryLatest` cache vs cold-disk paths, multi-tier aggregation, and the inline downsampler.

### Cross-Compile

```bash
bash addons/build.sh cross    # builds amd64, arm64, riscv64
```

### Debian / Ubuntu (.deb)

```bash
bash addons/build_deb.sh
sudo dpkg -i dist/kula_*.deb
```

### Arch Linux (AUR)

```bash
bash addons/build_aur.sh
cd dist/aur && makepkg -si
```

### Docker

```bash
bash addons/docker/build.sh
docker compose -f addons/docker/docker-compose.yml up -d
```

---

## Project Structure

```
kula/
├── cmd/
│   ├── kula/
│   │   └── main.go             # CLI entry point, flag parsing, commands
│   └── gen-mock-data/
│       └── main.go             # Mock data generator utility
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
│   ├── sandbox/                # Linux Landlock sandboxing
│   ├── storage/                # Tiered ring-buffer engine
│   │   ├── store.go            #   Multi-tier coordinator + aggregation
│   │   ├── tier.go             #   Single ring-buffer file
│   │   └── codec.go            #   JSON encode/decode for samples
│   ├── tui/                    # Terminal UI (bubbletea + lipgloss)
│   └── web/                    # HTTP/WebSocket server
│       ├── server.go           #   Routes, API handlers, startup
│       ├── websocket.go        #   Live streaming hub + client mgmt
│       ├── auth.go             #   Argon2id auth, sessions, middleware
│       └── static/             #   Embedded SPA (served from binary)
│           ├── index.html      #     Dashboard markup
│           ├── app.js          #     Charts, WebSocket, UI logic
│           └── style.css       #     Dark theme, glassmorphism
├── addons/
│   ├── inspect_tier.py         # Standalone Python script for reading tiers
│   ├── benchmark.sh            # Storage engine benchmark suite (formatted output)
│   ├── build.sh                # Cross-compile (amd64/arm64/riscv64)
│   ├── build_deb.sh            # Debian package builder
│   ├── build_aur.sh            # Arch AUR PKGBUILD generator
│   ├── check.sh                # Linting + testing
│   ├── kula.1                  # Man page
│   ├── release.sh              # CI packaging wrapper
│   ├── bash-completion/        # Bash completion script
│   ├── docker/                 # Dockerfile + compose
│   └── init/                   # Service files (systemd/openrc/runit)
├── config.example.yaml
├── VERSION                     # Single source of truth for version
├── CHANGELOG
└── LICENSE                     # AGPL-3.0
```

---

## License

[GNU Affero General Public License v3.0](LICENSE)

---

## Attributions

- Linux® is the registered trademark of Linus Torvalds in the U.S. and other countries.
- [Chart.js](https://www.chartjs.org/) library licensed under MIT
