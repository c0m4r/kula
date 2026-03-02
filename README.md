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

<img width="1037" height="793" alt="image" src="https://github.com/user-attachments/assets/909b25c6-ebba-43ce-b8f6-a82cc30ab166" />

</div>

---

## What It Does

Kula collects system metrics every second by reading directly from `/proc` and `/sys`, stores them in a built-in tiered ring-buffer storage engine, and serves them through a real-time Web UI dashboard and a terminal TUI.

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

### Quick

```bash
wget -O kula https://github.com/c0m4r/kula/releases/latest/download/kula-linux-amd64
chmod +x kula
./kula
```

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
Usage:
  kula [flags] [command]

Commands:
  serve          Start the monitoring daemon with web UI (default)
  tui            Launch the terminal UI dashboard
  hash-password  Generate an Argon2 password hash for config

Flags:
  -config string  Path to configuration file (default "config.yaml")
  -h, --help      Show this help message

Examples:
  kula                              Start with default config
  kula -config /etc/kula/config.yaml serve
  kula tui
  kula hash-password
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
│       ├── auth.go             #   Argon2id auth, sessions, middleware
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
# Lint + test suite
bash ./addons/check.sh

# Build dev (Binary size: ~11MB)
CGO_ENABLED=0 go build -o kula ./cmd/kula/

# Build prod (Binary size: ~8MB)
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -buildvcs=false -o kula ./cmd/kula/

```

---

## License

[GNU Affero General Public License v3.0](LICENSE)

Linux® is the registered trademark of Linus Torvalds in the U.S. and other countries.
