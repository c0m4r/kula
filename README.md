<div align="center">

<img width="128" height="128" alt="image" src="https://github.com/user-attachments/assets/77d5850a-c3a4-47fe-b33e-dcaeeb3c8e4d" />

# K U L A

**Lightweight, self-contained Linux® server monitoring tool.**

![Linux](https://img.shields.io/badge/made%20for-linux-yellow?logo=linux&logoColor=ffffff)
![Go](https://img.shields.io/badge/go%20go-power%20rangers-blue?logo=go&logoColor=ffffff)
![JS](https://img.shields.io/badge/some%20-js-orange?logo=javascript&logoColor=ffffff)
![Bash](https://img.shields.io/badge/and%20a%20pinch%20of-bash-green?logo=linux&logoColor=ffffff)
[![License: GPL v3](https://img.shields.io/badge/License-AGPLv3-red.svg)](https://www.gnu.org/licenses/agpl-3.0)

[🌏 Website](https://kula.ovh) | [👀 Demo](https://demo.kula.ovh/) | [🐋 Docker Hub](https://hub.docker.com/r/c0m4r/kula)

Zero dependencies. No external databases. Single binary. Just deploy and go.

<img width="1011" height="834" alt="image" src="https://github.com/user-attachments/assets/771b3e95-8713-44d2-8309-cd9e1f722a7e" />

</div>

---

## 📦 What It Does

Kula collects system metrics every second by reading directly from `/proc` and `/sys`, 
stores them in a built-in tiered ring-buffer storage engine, and serves them through a real-time Web UI dashboard and a terminal TUI.

| Metric | What's Collected |
|--------|-----------------|
| **CPU** | Total usage (user, system, iowait, irq, softirq, steal) + core count |
| **GPU** | Load, Power consumption, VRAM |
| **Load** | 1 / 5 / 15 min averages, running & total tasks |
| **Memory** | Total, free, available, used, buffers, cached, shmem |
| **Swap** | Total, free, used |
| **Network** | Per-interface throughput (Mbps), packets/s, errors, drops; TCP errors/s, resets/s, retrans, established; sockets |
| **Disks** | Per-device I/O (read/write bytes/s, reads/s, writes/s IOPS); filesystem usage |
| **System** | Uptime, entropy, clock sync, hostname, logged-in user count |
| **Processes** | Running, sleeping, blocked, zombie counts |
| **Self** | Kula's own CPU%, RSS memory, open file descriptors |
| **Thermal** | CPU, GPU and Disk temperatures |
| **Battery** | /sys/class/power_supply - power supply / battery status |
| **Containers** | Docker, podman, raw cgroups |
| **Applications** | PostgreSQL, MySQL/MariaDB, nginx, apache2 |
| **Custom** | Monitor anything with custom metrics |

Note: Monitoring NVIDIA GPUs might require additional setup. Check [GPU monitoring](https://github.com/c0m4r/kula/wiki/GPU-monitoring).

---

## 🪩 How It Works

```
    ╭──────────────────────────────────────────────╮
    │                  Linux Kernel                │
    │      /proc/stat  /proc/meminfo  /sys/...     │
    ╰───────────────────────┬──────────────────────╯
                            │ Read every 1s
                            ▼
    ╭──────────────────────────────────────────────╮
    │                   Collectors                 │
    │        (CPU, Mem, Net, Disk, System)         │
    ╰───────────────────────┬──────────────────────╯
                            │ Live Data
         ╭──────────────────┼─────────────────────╮
         ▼                  ▼                     ▼
╭─────────────────╮  ╭────────────────╮  ╭─────────────────╮
│ Storage Engine  │  │   Web Server   │  │   TUI Terminal  │
╰───┬─────────┬───╯  ╰──────┬─────────╯  ╰─────────────────╯
    │         │             │
    │         ╰──(History)──┤              ╭───────────────╮
    │                       ╰──(HTTP/WS)─► |   Dashboard   |
    ▼                                      ╰───────────────╯
╭──────────┬──────────┬──────────╮
│  Tier 1  │  Tier 2  │  Tier 3  │
│    1s    │    1m    │    5m    │
│  250 MB  │  150 MB  │  50 MB   │
╰──────────┴──────────┴──────────╯
 Ring-buffer binary files
 with circular overwrites
```

### Storage Engine

Kula is powered by a custom-built, high-performance **ring-buffer** storage system that writes metrics directly into fixed-size binary files. Because the files have a strict maximum capacity, new data seamlessly wraps around to overwrite the oldest entries. On startup, Kula restores the latest-sample cache and reconstructs any pending aggregation buffers so it can resume serving recent data and continue tier rollups after a restart.

To maximize efficiency, Kula employs a multi-tiered architecture that intelligently downsamples older data:

- **Tier 1** — Raw 1-second samples (default 250 MB)
- **Tier 2** — 1-minute metrics aggregation (Avg/Min/Max) (default 150 MB)
- **Tier 3** — 5-minute metrics aggregation (Avg/Min/Max) (default 50 MB)

### HTTP server

The HTTP server on backend exposes a REST API and a WebSocket endpoint for live streaming. 
Authentication is optional. When enabled, Kula uses Argon2id password hashing, secure session cookies, token-only session validation with sliding expiration, and hashed-at-rest session persistence. Authenticated API access can also use a bearer session token via the `Authorization` header.

### Dashboard

The frontend is a single-page application embedded in the binary. Built on Chart.js with custom SVG gauges, 
it connects via WebSocket for live updates and falls back to history API for longer time ranges. Features include:

- Interactive zoom with drag-select (auto-pauses live stream)
- Focus mode to display only specific charts of interest
- Configurable Y-axis bounds (Manual limits or Auto-detect)
- Per-device selectors for Network, Disk I/O, and Thermal monitoring
- Grid / stacked list layout toggle
- Alert system for clock sync, low entropy, and system overload
- Modern aesthetics with light/dark theme support
- Optional AI assistant powered by a local Ollama model (see below)
- Prometheus exporter endpoint for scraping into existing observability stacks

### AI Assistant

Kula features an AI assistant via [Ollama](https://github.com/ollama/ollama).

When Ollama is enabled in `config.yaml`, a 🤖 button appears in the dashboard header. The panel supports:

- **Multi-session conversations** — open independent threads and switch between them
- **Per-chart analysis** — click the 🤖 icon on any chart card to open a session pre-loaded with that chart's recent data as CSV
- **Agentic tool calling** — the model can call `get_metrics` to pull metrics on demand (up to 5 rounds per turn)
- **Model selector** — switch between any locally available Ollama model mid-session
- **Draggable & resizable panel** — drag by the header, resize from the bottom-right grip
- **Streaming responses** with markdown rendering

All AI inference runs locally through Ollama API.

---

## 💾 Installation

Kula was built to have everything in one binary file. You can just upload it to your server 
and not worry about installing anything else because Kula has no dependencies. It just works out of the box! 
It is a great tool when you need to quickly start real-time monitoring.

Example installation methods for **amd64 (x86_64)** GNU/Linux.

Check [Releases](https://github.com/c0m4r/kula/releases) for **ARM** and **RISC-V** packages.

Note: Never thoughtlessly paste commands into the terminal. Even checking the checksum is no substitute for reviewing the code.

### Guided

```bash
bash -c "$(curl -fsSL https://raw.githubusercontent.com/c0m4r/kula/refs/heads/main/addons/install_v2.sh)"
```

### Guided (verify installer)

```bash
KULA_INSTALL=$(mktemp)
curl -o ${KULA_INSTALL} -fsSL https://raw.githubusercontent.com/c0m4r/kula/refs/heads/main/addons/install_v2.sh
echo "bad61ee9eed4595d20fa7e613bd27c3b8700c67f8a5fcac756d282a811705398 ${KULA_INSTALL}" | sha256sum -c || rm -f ${KULA_INSTALL}
bash ${KULA_INSTALL}
rm -f ${KULA_INSTALL}
```

### Standalone

```bash
wget https://github.com/c0m4r/kula/releases/download/0.17.2/kula-0.17.2-amd64.tar.gz
echo "804b96337c4970f23f081620b804d0ff81a516c64f5b5c71e2fe2a68b2dab35a kula-0.17.2-amd64.tar.gz" | sha256sum -c || rm -f kula-0.17.2-amd64.tar.gz
tar -xvf kula-0.17.2-amd64.tar.gz
cd kula
./kula
```

### Docker

Temporary, no persistent storage:

```bash
docker run --rm -it --name kula --pid host --network host -v /proc:/proc:ro c0m4r/kula:latest
```

With persistent storage:

```bash
docker run -d --name kula --pid host --network host -v /proc:/proc:ro -v kula_data:/app/data c0m4r/kula:latest
docker logs -f kula
```

### Debian / Ubuntu (.deb)

```bash
wget https://github.com/c0m4r/kula/releases/download/0.17.2/kula-0.17.2-amd64.deb
echo "72775325e178d85e2dd4ff967327559e3f7f8d0275f2db9c7c19f46c7410b175 kula-0.17.2-amd64.deb" | sha256sum -c || rm -f kula-0.17.2-amd64.deb
sudo dpkg -i kula-0.17.2-amd64.deb
journalctl -f -t kula
```

### RHEL / Fedora / CentOS / Rocky / Alma (.rpm)

```bash
wget https://github.com/c0m4r/kula/releases/download/0.17.2/kula-0.17.2-x86_64.rpm
echo "e311125504a77419f4832fa75e954dac6b8f7b1e932818f80d30bc3223b4a20f kula-0.17.2-x86_64.rpm" | sha256sum -c || rm -f kula-0.17.2-x86_64.rpm
sudo rpm -i kula-0.17.2-x86_64.rpm
journalctl -f -t kula
```

### Arch Linux / Manjaro (AUR)

https://aur.archlinux.org/packages/kula

```bash
git clone https://aur.archlinux.org/kula.git
cd kula
makepkg -si
```

### Snap

```bash
sudo snap install kula
```

The snap uses **strict sandbox** so by default Kula features will be limited to the basics, which can be extended with snap connect.

See [Snap Wiki](https://github.com/c0m4r/kula/wiki/Snap) for the full guide.

### Build from Source

```bash
git clone https://github.com/c0m4r/kula.git
cd kula
./addons/build.sh
```

---

## 💻 Usage

### Quick Start

Starting Kula is as simple as running:

```bash
./kula
```

Dashboard will be available at: http://localhost:27960 (or :8080 if you're using earlier versions)

You can change default port and listen address in [`config.yaml`](config.example.yaml) or using environment variables:

```bash
export KULA_LISTEN="127.0.0.1"
export KULA_PORT="27960"
./kula
```

### TUI

```bash
./kula tui
```

### Inspect storage

```bash
./kula inspect
```

### Prometheus metrics

See: [Prometheus metrics](https://github.com/c0m4r/kula/wiki/Prometheus-metrics) for more info.

### Health endpoints

Kula exposes lightweight liveness endpoints at:

```
http://localhost:27960/health
http://localhost:27960/status
```

Both return:

```
200 OK
kula is healthy
```

### Authentication (Optional)

```bash
# Generate password hash
./kula hash-password

# Add the output to config.yaml under web.auth
```

When authentication is enabled, Kula issues a random session token after login, stores only its hash on disk, and validates requests by token expiry/validity rather than binding sessions to client IP or User-Agent.

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

## ⚙️ Configuration

All settings live in `config.yaml`. See [`config.example.yaml`](config.example.yaml) for defaults.

---

## 🧰 Development

```bash
# Lint + test suite
./addons/check.sh

# Build
./addonsh.build.sh

# Build dev (Binary size: ~20MB)
CGO_ENABLED=0 go build -o kula ./cmd/kula/

# Build prod (Binary size: ~14MB, xz: ~4MB)
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -buildvcs=false -o kula ./cmd/kula/
```

### Updating Dependencies

To safely update only the Go modules used by Kula to their latest minor/patch versions, and prune any unused dependencies:

```bash
./addons/go_modules_updates.py
go get -u ./...
go mod tidy
```

### Testing & Benchmarks

```bash
# Run unit tests with race detector
go test -race ./...

# Run the full storage benchmark suite (default: 3s per bench)
./addons/benchmark.sh

# Python scripts formatter and linters
black addons/*.py
pylint addons/*.py
mypy --strict addons/*.py
```

### Cross-Compile

```bash
./addons/build.sh cross    # builds amd64, arm64, riscv64
```

### Debian / Ubuntu (.deb)

```bash
./addons/build_deb.sh
ls -1 dist/kula-*.deb
```

### Arch Linux / Manjaro (AUR)

```bash
./addons/build_aur.sh
cd dist/aur && makepkg -si
```

### RHEL / Fedora / CentOS / Rocky / Alma (.rpm)

```bash
./addons/build_rpm.sh
ls -1 dist/kula-*.rpm
```

### Docker

```bash
./addons/docker/build.sh
docker compose -f addons/docker/docker-compose.yml up -d
```

### Snap

Requires [snapcraft](https://documentation.ubuntu.com/snapcraft/stable/howto/setup/)
and a build backend (LXD recommended):

```bash
snap install snapcraft --classic
snap install lxd
lxd init --auto
./addons/build_snap.sh            # host arch, into dist/
./addons/build_snap.sh cross      # cross-build amd64/arm64/riscv64 locally
ls -1 dist/kula-*.snap
```

---

## 🔒 Privacy

Privacy is a core pillar, not an afterthought.

Kula is built for privacy-conscious infrastructure. It is a completely self-contained binary that requires no cloud connection and no third-party APIs. Designed to function perfectly in air-gapped networks, Kula never sends metadata to external servers, never serves advertisements, and requires no user registration. Your monitoring starts and ends on your infrastructure, exactly where it should be.

---

## 📖 License

[GNU Affero General Public License v3.0](LICENSE)

---

## 🫶 Attributions

- [Linux®](https://github.com/torvalds/linux) is the registered trademark of Linus Torvalds in the U.S. and other countries.
- [Chart.js](https://www.chartjs.org/) library licensed under MIT
- [Inter](https://github.com/rsms/inter) font by Rasmus Andersson licensed under [OFL-1.1](https://openfontlicense.org/)
- [Press Start 2P](https://fonts.google.com/specimen/Press+Start+2P?query=CodeMan38) font by CodeMan38 licensed under [OFL-1.1](https://openfontlicense.org/)
