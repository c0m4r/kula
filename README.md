<div align="center">

<img width="128" alt="Kula" src="https://kula.ovh/kula.svg" />

# K U L A

**Lightweight, self-contained Linux® server monitoring tool.**

![Linux](https://img.shields.io/badge/made%20for-linux-yellow?logo=linux&logoColor=ffffff)
![Go](https://img.shields.io/badge/go%20go-power%20rangers-blue?logo=go&logoColor=ffffff)
![JS](https://img.shields.io/badge/some%20-js-orange?logo=javascript&logoColor=ffffff)
![Bash](https://img.shields.io/badge/and%20a%20pinch%20of-bash-green?logo=linux&logoColor=ffffff)
[![License: GPL v3](https://img.shields.io/badge/License-AGPLv3-red.svg)](https://www.gnu.org/licenses/agpl-3.0)

[💾 Installation](https://github.com/c0m4r/kula#-installation) | [🌏 Website](https://kula.ovh) | [👀 Demo](https://demo.kula.ovh/) | [🐋 Docker Hub](https://hub.docker.com/r/c0m4r/kula)

Zero dependencies. No external databases. Single binary. Just deploy and go.


<a href="https://kula.ovh#gh-light-mode-only">
  <img src="https://kula.ovh/light.png" alt="Kula" />
</a>
<a href="https://kula.ovh#gh-dark-mode-only">
  <img src="https://kula.ovh/dark.png" alt="Kula" />
</a>

</div>

## 📦 What It Does

Kula reads system metrics every second directly from `/proc` and `/sys`, stores them in a
built-in tiered ring-buffer storage engine, and serves them through a real-time web dashboard,
a terminal UI and a Prometheus endpoint.

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

Monitoring NVIDIA GPUs might require additional setup — see [GPU troubleshooting][gpu].

- **[Web dashboard][dashboard]** — live charts, pan and zoom through history, Min–Max bands,
  Focus and TV modes, rearrangeable cards, a System Info hardware page, and 26 languages.
- **[Terminal UI][tui]** — a fast live read over SSH, including charts of stored history.
- **[Tiered storage][how]** — fixed-size ring-buffer files keep raw 1-second samples, then
  1-minute and 5-minute rollups. No database to run.
- **[Application monitoring][apps]** — nginx, Apache2, PostgreSQL, MySQL/MariaDB and
  containers, plus your own [custom metrics][custom].
- **[Prometheus exporter][prometheus]** — scrape Kula into an existing observability stack.
- **[AI assistant][ai]** — optional chat about your metrics, powered by a local Ollama model.
- **[Authentication][auth]** — optional Argon2id login, with multiple users.
- **[Backups][backups]** — scheduled snapshots of the storage tiers.
- **Secure by default** — strict headers, CSRF protection, rate limits, DNS rebinding
  protection, and a [Landlock sandbox][sandbox] that confines file and network access. See the
  [security model][security].

## ⚙️ How It Works

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
│  Tier 0  │  Tier 1  │  Tier 2  │
│    1s    │    1m    │    5m    │
│  250 MB  │  150 MB  │  50 MB   │
╰──────────┴──────────┴──────────╯
 Ring-buffer binary files
 with circular overwrites
```

### Storage Engine

Kula is powered by a custom-built, high-performance **ring-buffer** storage system that writes metrics directly into fixed-size binary files. Because the files have a strict maximum capacity, new data seamlessly wraps around to overwrite the oldest entries.

To maximize efficiency, Kula employs a multi-tiered architecture that intelligently downsamples older data:

- **Tier 0** — Raw 1-second samples (default 250 MB)
- **Tier 1** — 1-minute metric rollups (default 150 MB)
- **Tier 2** — 5-minute metric rollups (default 50 MB)

## 💾 Installation

Kula is a single binary with no dependencies: upload it to a server and run it. Release
packages cover **amd64**, **arm64** and **riscv64** — see
[Releases](https://github.com/c0m4r/kula/releases).

Note: Never thoughtlessly paste commands into the terminal. Even checking the checksum is no substitute for reviewing the code.


### Guided

```bash
bash -c "$(curl -fsSL https://raw.githubusercontent.com/c0m4r/kula/refs/heads/main/addons/install_v3.sh)"
```

### Guided (verify installer)

```bash
KULA_INSTALL=$(mktemp)
curl -o "${KULA_INSTALL}" -fsSL https://raw.githubusercontent.com/c0m4r/kula/refs/heads/main/addons/install_v3.sh
echo "1f90556010a7e427525b2c919f9bcedc406a7de1a81a78bcd6baeefb5852494e ${KULA_INSTALL}" | sha256sum -c && bash "${KULA_INSTALL}"
rm -f "${KULA_INSTALL}"
```

### Standalone

```bash
wget https://github.com/c0m4r/kula/releases/download/0.22.0/kula-0.22.0-amd64.tar.gz
echo "5faada3a6a187b7f850c0556cd4e3dc0b6df56ddfb9c9a460b3cccd12e35405d kula-0.22.0-amd64.tar.gz" | sha256sum -c && tar -xvf kula-0.22.0-amd64.tar.gz
cd kula
./kula
```

### Docker

Kula needs the host's PID and network namespaces and a read-only `/proc`, and no capabilities.

Temporary, no persistent storage:

```bash
docker run --rm -it --name kula --pid host --network host \
  --cap-drop ALL --security-opt no-new-privileges:true \
  -v /proc:/proc:ro c0m4r/kula:latest
```

With persistent storage:

```bash
docker run -d --name kula --pid host --network host \
  --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  -v /proc:/proc:ro -v kula_data:/app/data c0m4r/kula:latest
docker logs -f kula
```

To count logged-in users, also mount the host's utmp or systemd-logind sessions, whichever it
has, read-only. `--mount` refuses a source the host does not have (`-v` would create it as a
directory); drop that line instead:

```bash
docker run -d --name kula --pid host --network host \
  --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  -v /proc:/proc:ro -v kula_data:/app/data \
  --mount type=bind,source=/run/utmp,target=/run/utmp,readonly \
  --mount type=bind,source=/run/systemd/sessions,target=/run/systemd/sessions,readonly \
  c0m4r/kula:latest
```

More in [Installation → Docker][install-docker].

### Debian / Ubuntu (.deb)

```bash
wget https://github.com/c0m4r/kula/releases/download/0.22.0/kula-0.22.0-amd64.deb
echo "d5e06e1f6e58d35fa245163e70571d61c2b601ba79038955dc9d980425372b8e kula-0.22.0-amd64.deb" | sha256sum -c && sudo dpkg -i kula-0.22.0-amd64.deb
journalctl -f -t kula
```

### RHEL / Fedora / CentOS / Rocky / Alma (.rpm)

```bash
wget https://github.com/c0m4r/kula/releases/download/0.22.0/kula-0.22.0-x86_64.rpm
echo "81c98938afc722e446cd07832f3f5cd0629e65337f0df0801d3a896c56de8792 kula-0.22.0-x86_64.rpm" | sha256sum -c && sudo rpm -i kula-0.22.0-x86_64.rpm
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
Its daemon runs as root, bounded by that sandbox rather than an unprivileged user.

See [Snap Wiki](https://github.com/c0m4r/kula/wiki/Snap) for the full guide.

### Build from Source

Requires [Go](https://go.dev/).

```bash
git clone https://github.com/c0m4r/kula.git
cd kula
./addons/build.sh
```

## 💻 Usage

### Quick Start

Starting Kula is as simple as running:

```bash
./kula
```

Dashboard will be available at: http://localhost:27960

You can change default port and listen address in [`config.yaml`](config.example.yaml) or using environment variables:

```bash
export KULA_LISTEN="127.0.0.1"
export KULA_PORT="27960"
./kula
```

The default command is `serve` (`./kula serve`). Global flags are `-config <path>` to
select another configuration file and `-version` (or `-v`) to print the version.

Every command and flag: [CLI reference][cli].

### TUI

```bash
./kula tui
```

### Prometheus metrics

See: [Prometheus metrics](https://github.com/c0m4r/kula/wiki/Prometheus-metrics) for more info.

### Authentication (Optional)

```bash
# Generate password hash
./kula hash-password

# Add the output to config.yaml under web.auth
```

## ⚙️ Configuration

All settings live in `config.yaml`. [`config.example.yaml`](config.example.yaml) lists every
option with its default, and the [configuration reference][config] explains each one along
with the environment variables that override them.

## 📚 Documentation

The [documentation][docs] has a user guide and a developer guide:

- **Get started:** [Introduction][intro] · [Installation][install] · [Quick start][quick] ·
  [Configuration][config]
- **Use:** [Web dashboard][dashboard] · [Terminal UI][tui] · [CLI reference][cli] ·
  [Troubleshooting][troubleshooting]
- **Integrate:** [Application monitoring][apps] · [Custom metrics][custom] ·
  [Prometheus][prometheus] · [AI assistant][ai]
- **Operate:** [Authentication][auth] · [Backups][backups] · [Reverse proxy & TLS][proxy] ·
  [Service management][service]
- **Develop:** [Architecture][arch] · [Building][building] · [Testing][testing] ·
  [Adding a metric][metrics] · [Packaging][packaging] · [Contributing][contributing]

More operational guides are on the [wiki](https://github.com/c0m4r/kula/wiki).

## 🧰 Development

```bash
./addons/check.sh         # full gate: govulncheck, gofmt, vet, race tests, golangci-lint
./addons/build.sh         # build for the current architecture
./addons/build.sh cross   # cross-compile amd64, arm64, riscv64
```

[Building & toolchain][building] covers dependency updates and running the CI workflows
locally; [Packaging & release][packaging] covers the .deb, .rpm, AUR, Snap and Docker builds.

## 🔒 Privacy

Privacy is a core pillar, not an afterthought.

Kula is built for privacy-conscious infrastructure. It is a completely self-contained binary that requires no cloud connection and no third-party APIs. Designed to function perfectly in air-gapped networks, Kula never sends metadata to external servers, never serves advertisements, and requires no user registration. Your monitoring starts and ends on your infrastructure, exactly where it should be.

## 📖 License

[GNU Affero General Public License v3.0](LICENSE)

## 🫶 Attributions

- [Linux®](https://github.com/torvalds/linux) is the registered trademark of Linus Torvalds in the U.S. and other countries.
- [Chart.js](https://www.chartjs.org/) library licensed under [MIT](https://github.com/chartjs/Chart.js/blob/master/LICENSE.md)
- [Inter](https://github.com/rsms/inter) font by Rasmus Andersson licensed under [OFL-1.1](https://openfontlicense.org/)
- [Press Start 2P](https://fonts.google.com/specimen/Press+Start+2P?query=CodeMan38) font by CodeMan38 licensed under [OFL-1.1](https://openfontlicense.org/)

[docs]: https://github.com/c0m4r/kula/blob/main/docs/README.md
[intro]: https://github.com/c0m4r/kula/blob/main/docs/user/01-introduction.md
[how]: https://github.com/c0m4r/kula/blob/main/docs/user/01-introduction.md#how-it-works
[install]: https://github.com/c0m4r/kula/blob/main/docs/user/02-installation.md
[install-verify]: https://github.com/c0m4r/kula/blob/main/docs/user/02-installation.md#verifying-the-installer-first
[install-tarball]: https://github.com/c0m4r/kula/blob/main/docs/user/02-installation.md#standalone-binary-tarball
[install-docker]: https://github.com/c0m4r/kula/blob/main/docs/user/02-installation.md#docker
[install-deb]: https://github.com/c0m4r/kula/blob/main/docs/user/02-installation.md#debian--ubuntu-deb
[install-rpm]: https://github.com/c0m4r/kula/blob/main/docs/user/02-installation.md#rhel--fedora--centos--rocky--alma-rpm
[install-aur]: https://github.com/c0m4r/kula/blob/main/docs/user/02-installation.md#arch-linux--manjaro-aur
[install-snap]: https://github.com/c0m4r/kula/blob/main/docs/user/02-installation.md#snap
[install-source]: https://github.com/c0m4r/kula/blob/main/docs/user/02-installation.md#build-from-source
[quick]: https://github.com/c0m4r/kula/blob/main/docs/user/03-quick-start.md
[config]: https://github.com/c0m4r/kula/blob/main/docs/user/04-configuration.md
[dashboard]: https://github.com/c0m4r/kula/blob/main/docs/user/05-web-dashboard.md
[tui]: https://github.com/c0m4r/kula/blob/main/docs/user/06-tui.md
[auth]: https://github.com/c0m4r/kula/blob/main/docs/user/07-authentication.md
[apps]: https://github.com/c0m4r/kula/blob/main/docs/user/08-application-monitoring.md
[custom]: https://github.com/c0m4r/kula/blob/main/docs/user/09-custom-metrics.md
[ai]: https://github.com/c0m4r/kula/blob/main/docs/user/10-ai-assistant.md
[prometheus]: https://github.com/c0m4r/kula/blob/main/docs/user/11-prometheus.md
[backups]: https://github.com/c0m4r/kula/blob/main/docs/user/12-backups.md
[proxy]: https://github.com/c0m4r/kula/blob/main/docs/user/13-reverse-proxy.md
[cli]: https://github.com/c0m4r/kula/blob/main/docs/user/14-cli-reference.md
[service]: https://github.com/c0m4r/kula/blob/main/docs/user/15-service-management.md
[troubleshooting]: https://github.com/c0m4r/kula/blob/main/docs/user/16-troubleshooting.md
[gpu]: https://github.com/c0m4r/kula/blob/main/docs/user/16-troubleshooting.md#gpu-not-detected-especially-nvidia
[arch]: https://github.com/c0m4r/kula/blob/main/docs/dev/01-architecture.md
[building]: https://github.com/c0m4r/kula/blob/main/docs/dev/03-building.md
[security]: https://github.com/c0m4r/kula/blob/main/docs/dev/08-security.md
[sandbox]: https://github.com/c0m4r/kula/blob/main/docs/dev/09-sandbox.md
[testing]: https://github.com/c0m4r/kula/blob/main/docs/dev/12-testing.md
[metrics]: https://github.com/c0m4r/kula/blob/main/docs/dev/14-adding-metrics.md
[packaging]: https://github.com/c0m4r/kula/blob/main/docs/dev/15-packaging.md
[contributing]: https://github.com/c0m4r/kula/blob/main/docs/dev/16-contributing.md
