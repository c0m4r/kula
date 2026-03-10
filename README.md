<div align="center">

<img width="128" height="128" alt="image" src="https://github.com/user-attachments/assets/77d5850a-c3a4-47fe-b33e-dcaeeb3c8e4d" />

# K U L A

**Lightweight, self-contained Linux® server monitoring tool.**

![Go](https://img.shields.io/badge/made%20for-linux-yellow?logo=linux&logoColor=ffffff)
![Go](https://img.shields.io/badge/go%20go-power%20rangers-blue?logo=go&logoColor=ffffff)
![JS](https://img.shields.io/badge/some%20-js-orange?logo=javascript&logoColor=ffffff)
![JS](https://img.shields.io/badge/and%20a%20pinch%20of-bash-green?logo=linux&logoColor=ffffff)
[![License: GPL v3](https://img.shields.io/badge/License-AGPLv3-red.svg)](https://www.gnu.org/licenses/agpl-3.0)

[👀 Demo](https://demo.kula.ovh/) | [🐋 Docker Hub](https://hub.docker.com/r/c0m4r/kula)

Zero dependencies. No external databases. Single binary. Just deploy and go.

<img width="1037" height="793" alt="image" src="https://github.com/user-attachments/assets/d50e5653-0e84-4374-9ded-4d8e9f8eedd2" />

</div>

---

## 📦 What It Does

Kula collects system metrics every second by reading directly from `/proc` and `/sys`, 
stores them in a built-in tiered ring-buffer storage engine, and serves them through a real-time Web UI dashboard and a terminal TUI.

| Metric | What's Collected |
|--------|-----------------|
| **CPU** | Total usage (user, system, iowait, irq, softirq, steal) + core count, temperature |
| **Load** | 1 / 5 / 15 min averages, running & total tasks |
| **Memory** | Total, free, available, used, buffers, cached, shmem |
| **Swap** | Total, free, used |
| **Network** | Per-interface throughput (Mbps), packets/s, errors, drops; TCP errors/s, resets/s, established connections; socket counts |
| **Disks** | Per-device I/O (read/write bytes/s, reads/s, writes/s IOPS); filesystem usage |
| **System** | Uptime, entropy, clock sync, hostname, logged-in user count |
| **Processes** | Running, sleeping, blocked, zombie counts |
| **Self** | Kula's own CPU%, RSS memory, open file descriptors |

---

## 🪩 How It Works

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

Data is persisted in **pre-allocated ring-buffer files** per tier. Each tier file has a fixed maximum size — when it fills up, 
new data overwrites the oldest entries. This gives predictable, bounded disk usage with no cleanup needed.

- **Tier 1** — Raw 1-second samples (default 250 MB)
- **Tier 2** — 1-minute aggregates: averaged CPU & network, last-value gauges (default 150 MB)
- **Tier 3** — 5-minute aggregates, same logic (default 50 MB)

### HTTP server

The HTTP server on backend exposes a REST API and a WebSocket endpoint for live streaming. 
Authentication is optional - when enabled, it uses Argon2id hashing with salt and session cookies. 
It is worth adding that Kula truly respects your privacy. It works on closed networks and does not make any calls to external services.

### Dashboard

The frontend is a single-page application embedded in the binary. Built on Chart.js with custom SVG gauges, 
it connects via WebSocket for live updates and falls back to history API for longer time ranges. Features include:

- Interactive zoom with drag-select (auto-pauses live stream)
- Focus mode to show only selected graphs
- Grid / stacked list layout toggle
- Alert system for clock sync, entropy issues, overload
- Light and dark themes available
---

## 💾 Installation

Kula was built to have everything in one binary file. You can just upload it to your server 
and not worry about installing anything else because Kula has no dependencies. It just works out of the box! 
It is a great tool when you need to quickly start real-time monitoring.

Example installation methods for **amd64 (x86_64)** GNU/Linux.

Check [Releases](https://github.com/c0m4r/kula/releases) for **ARM** and **RISC-V** packages.

### Quick (auto-detect)

```bash
KULA_INSTALL=$(mktemp)
curl -o ${KULA_INSTALL} -fsSL https://kula.ovh/install
echo "411f4ab2f4023b23d31545ef4f647a3720dbd9c7660dd3ab959f93d5414c29ca ${KULA_INSTALL}" | sha256sum -c || rm -f ${KULA_INSTALL}
bash ${KULA_INSTALL}
rm -f ${KULA_INSTALL}
```

### Standalone

```bash
wget https://github.com/c0m4r/kula/releases/download/0.7.5/kula-0.7.5-amd64.tar.gz
echo "d0ed586b243ccd7abd3c4733fe3ad02e55d74139977110227e90ece100d68ec1 kula-0.7.5-amd64.tar.gz" | sha256sum -c || rm -f kula-0.7.5-amd64.tar.gz
tar -xvf kula-0.7.5-amd64.tar.gz
cd kula
./kula
```

### Docker

```bash
# With persistent storage
docker run -d --name kula --pid host --network host -v /proc:/proc:ro -v kula_data:/app/data c0m4r/kula:latest
docker logs -f kula

# Temporary, no persistent storage
docker run --rm -it --name kula --pid host --network host -v /proc:/proc:ro c0m4r/kula:latest
```

### Debian / Ubuntu (.deb)

```bash
wget https://github.com/c0m4r/kula/releases/download/0.7.5/kula-0.7.5-amd64.deb
echo "7c6d9acc77e46d2356280d966006f13aed1cfa6b1072daa6a02ed186852356d1 kula-0.7.5-amd64.deb" | sha256sum -c || rm -f kula-0.7.5-amd64.deb
sudo dpkg -i kula-0.7.5-amd64.deb
systemctl status kula
```

### RHEL / Fedora / CentOS / Rocky / Alma (.rpm)

```bash
wget https://github.com/c0m4r/kula/releases/download/0.7.5/kula-0.7.5-x86_64.rpm
echo "116dec0f4d166a7a759fee150a6ce18d4f57b1aa710f5f953bb7dc726363b5ce kula-0.7.5-x86_64.rpm" | sha256sum -c || rm -f kula-0.7.5-x86_64.rpm
sudo rpm -i kula-0.7.5-x86_64.rpm
systemctl status kula
```

### Arch Linux / Manjaro (AUR)

```bash
wget https://github.com/c0m4r/kula/releases/download/0.7.5/kula-0.7.5-aur.tar.gz
echo "7bb49b7ed212ff05749e4fddd8822f05b910842cc29d216aa835d73dbeec3d2b kula-0.7.5-aur.tar.gz" | sha256sum -c || rm -f kula-0.7.5-aur.tar.gz
tar -xvf kula-0.7.5-aur.tar.gz 
cd kula-0.7.5-aur
makepkg -si
```

### Build from Source

```bash
git clone https://github.com/c0m4r/kula.git
cd kula
bash addons/build.sh
```

---

## 💻 Usage

### Quick Start

```bash
# 1. Copy and edit config (optional)
cp config.example.yaml config.yaml

# 2. Start the server
./kula serve
# Dashboard at http://127.0.0.1:8080

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

## ⚙️ Configuration

All settings live in `config.yaml`. See [`config.example.yaml`](config.example.yaml) for defaults.

---

## 🧰 Development

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

### Cross-Compile

```bash
bash addons/build.sh cross    # builds amd64, arm64, riscv64
```

### Debian / Ubuntu (.deb)

```bash
bash addons/build_deb.sh
ls -1 dist/kula-*.deb
```

### Arch Linux / Manjaro (AUR)

```bash
bash addons/build_aur.sh
cd dist/aur && makepkg -si
```

### RHEL / Fedora / CentOS / Rocky / Alma (.rpm)

```bash
bash addons/build_rpm.sh
ls -1 dist/kula-*.rpm
```

### Docker

```bash
bash addons/docker/build.sh
docker compose -f addons/docker/docker-compose.yml up -d
```

---

## 📖 License

[GNU Affero General Public License v3.0](LICENSE)

---

## 🫶 Attributions

- Linux® is the registered trademark of Linus Torvalds in the U.S. and other countries.
- [Chart.js](https://www.chartjs.org/) library licensed under MIT
