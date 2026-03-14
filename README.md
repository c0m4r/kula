<div align="center">

<img width="128" height="128" alt="image" src="https://github.com/user-attachments/assets/77d5850a-c3a4-47fe-b33e-dcaeeb3c8e4d" />

# K U L A

**Lightweight, self-contained Linux® server monitoring tool.**

![Go](https://img.shields.io/badge/made%20for-linux-yellow?logo=linux&logoColor=ffffff)
![Go](https://img.shields.io/badge/go%20go-power%20rangers-blue?logo=go&logoColor=ffffff)
![JS](https://img.shields.io/badge/some%20-js-orange?logo=javascript&logoColor=ffffff)
![JS](https://img.shields.io/badge/and%20a%20pinch%20of-bash-green?logo=linux&logoColor=ffffff)
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
| **Load** | 1 / 5 / 15 min averages, running & total tasks |
| **Memory** | Total, free, available, used, buffers, cached, shmem |
| **Swap** | Total, free, used |
| **Network** | Per-interface throughput (Mbps), packets/s, errors, drops; TCP errors/s, resets/s, established connections; socket counts |
| **Disks** | Per-device I/O (read/write bytes/s, reads/s, writes/s IOPS); filesystem usage |
| **System** | Uptime, entropy, clock sync, hostname, logged-in user count |
| **Processes** | Running, sleeping, blocked, zombie counts |
| **Self** | Kula's own CPU%, RSS memory, open file descriptors |
| **Thermal** | CPU and Disk temperatures |

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
- **Tier 2** — 1-minute metrics aggregation (Avg/Min/Max) (default 150 MB)
- **Tier 3** — 5-minute metrics aggregation (Avg/Min/Max) (default 50 MB)

### HTTP server

The HTTP server on backend exposes a REST API and a WebSocket endpoint for live streaming. 
Authentication is optional - when enabled, it uses Argon2id hashing with salt and session cookies. 
It is worth adding that Kula truly respects your privacy. It works on closed networks and does not make any calls to external services.

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
sh -c "$(curl -fsSL https://raw.githubusercontent.com/c0m4r/kula/refs/heads/main/addons/install.sh)"
```

### Guided (verify installer)

```bash
KULA_INSTALL=$(mktemp)
curl -o ${KULA_INSTALL} -fsSL https://kula.ovh/install
echo "f0c064b20d23c948a4569a35cfe65589a36a497aa0d9037413c6e452471355dd ${KULA_INSTALL}" | sha256sum -c || rm -f ${KULA_INSTALL}
bash ${KULA_INSTALL}
rm -f ${KULA_INSTALL}
```

### Standalone

```bash
wget https://github.com/c0m4r/kula/releases/download/0.9.0/kula-0.9.0-amd64.tar.gz
echo "4a51a6772bd04af0526f992126769381145b62884dd91f4d461f4d56d6f85dbf kula-0.9.0-amd64.tar.gz" | sha256sum -c || rm -f kula-0.9.0-amd64.tar.gz
tar -xvf kula-0.9.0-amd64.tar.gz
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
wget https://github.com/c0m4r/kula/releases/download/0.9.0/kula-0.9.0-amd64.deb
echo "1f627ec783256899a9d7d4d40db6182cdedb5c2b1aa0ab5fe69b942d86f7cf81 kula-0.9.0-amd64.deb" | sha256sum -c || rm -f kula-0.9.0-amd64.deb
sudo dpkg -i kula-0.9.0-amd64.deb
journalctl -f -t kula
```

### RHEL / Fedora / CentOS / Rocky / Alma (.rpm)

```bash
wget https://github.com/c0m4r/kula/releases/download/0.9.0/kula-0.9.0-x86_64.rpm
echo "54e28ebe8ab02970304bce0f3fba8ab894184e0092beefd9d3fcd4e6117c11ad kula-0.9.0-x86_64.rpm" | sha256sum -c || rm -f kula-0.9.0-x86_64.rpm
sudo rpm -i kula-0.9.0-x86_64.rpm
journalctl -f -t kula
```

### Arch Linux / Manjaro (AUR)

```bash
wget https://github.com/c0m4r/kula/releases/download/0.9.0/kula-0.9.0-aur.tar.gz
echo "c790670f4846849c72d8104e50a7f5f72b83ad952d2c9815c2dbc5694db32d78 kula-0.9.0-aur.tar.gz" | sha256sum -c || rm -f kula-0.9.0-aur.tar.gz
tar -xvf kula-0.9.0-aur.tar.gz
cd kula-0.9.0-aur
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

Starting Kula is as simple as running:

```bash
./kula
```

Dashbord will be available at: http://localhost:27960 (or :8080 if you're using earlier versions)

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

# Build dev (Binary size: ~14MB)
CGO_ENABLED=0 go build -o kula ./cmd/kula/

# Build prod (Binary size: ~9MB, xz: ~3MB)
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
