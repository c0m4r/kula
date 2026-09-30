# Installation

Kula ships as a single binary with everything embedded. You can upload it to a server and run
it without installing anything else. The examples below use **amd64 (x86_64)**; check the
[Releases page](https://github.com/c0m4r/kula/releases) for **ARM** and **RISC-V** packages.

> ⚠️ **Security note.** Never paste install commands blindly. Verifying a checksum confirms a
> download is intact, but it is no substitute for reviewing the code you are about to run.

Package versions and checksums below are placeholders — take current values from the
[latest release](https://github.com/c0m4r/kula/releases/latest) and its
`CHECKSUMS.sha256.txt`. The installer checksum is the current hash of `install_v2.sh` on `main`.

---

## Guided installer (recommended)

The installer auto-detects your distro and offers the best packaging option, verifying every
download against the release's `CHECKSUMS.sha256.txt`.

```bash
bash -c "$(curl -fsSL https://raw.githubusercontent.com/c0m4r/kula/refs/heads/main/addons/install_v2.sh)"
```

### Verifying the installer first

```bash
KULA_INSTALL=$(mktemp)
curl -o ${KULA_INSTALL} -fsSL https://raw.githubusercontent.com/c0m4r/kula/refs/heads/main/addons/install_v2.sh
echo "bad61ee9eed4595d20fa7e613bd27c3b8700c67f8a5fcac756d282a811705398  ${KULA_INSTALL}" | sha256sum -c || rm -f ${KULA_INSTALL}
bash ${KULA_INSTALL}
rm -f ${KULA_INSTALL}
```

The installer supports flags:

| Flag | Meaning |
|------|---------|
| `-y`, `--yes` | Non-interactive; assume "yes" (downloads are still checksum-verified). |
| `--skip-verify` | Skip SHA-256 verification (not recommended). |
| `-h`, `--help` | Show help. |

---

## Standalone binary (tarball)

```bash
wget https://github.com/c0m4r/kula/releases/download/<version>/kula-<version>-amd64.tar.gz
echo "<sha256>  kula-<version>-amd64.tar.gz" | sha256sum -c || rm -f kula-<version>-amd64.tar.gz
tar -xvf kula-<version>-amd64.tar.gz
cd kula
./kula
```

---

## Docker

Kula needs host PID and network namespaces plus read-only `/proc` to see the host.

**Ephemeral (no persistent storage):**

```bash
docker run --rm -it --name kula --pid host --network host -v /proc:/proc:ro c0m4r/kula:latest
```

**With persistent storage:**

```bash
docker run -d --name kula --pid host --network host \
  -v /proc:/proc:ro -v kula_data:/app/data c0m4r/kula:latest
docker logs -f kula
```

A `docker-compose.yml` is provided in [`addons/docker/`](../../addons/docker/docker-compose.yml).
Images are published on [Docker Hub](https://hub.docker.com/r/c0m4r/kula) and GHCR.

---

## Debian / Ubuntu (.deb)

```bash
wget https://github.com/c0m4r/kula/releases/download/<version>/kula-<version>-amd64.deb
echo "<sha256>  kula-<version>-amd64.deb" | sha256sum -c || rm -f kula-<version>-amd64.deb
sudo dpkg -i kula-<version>-amd64.deb
journalctl -f -t kula
```

The package installs a systemd unit; Kula starts automatically.

---

## RHEL / Fedora / CentOS / Rocky / Alma (.rpm)

```bash
wget https://github.com/c0m4r/kula/releases/download/<version>/kula-<version>-x86_64.rpm
echo "<sha256>  kula-<version>-x86_64.rpm" | sha256sum -c || rm -f kula-<version>-x86_64.rpm
sudo rpm -i kula-<version>-x86_64.rpm
journalctl -f -t kula
```

---

## Arch Linux / Manjaro (AUR)

Package: https://aur.archlinux.org/packages/kula

```bash
git clone https://aur.archlinux.org/kula.git
cd kula
makepkg -si
```

---

## Snap

```bash
sudo snap install kula
```

The snap uses a **strict sandbox**, so by default Kula's capabilities are limited to the
basics. Extend them with `snap connect`. See the
[Snap wiki page](https://github.com/c0m4r/kula/wiki/Snap) for the full connection guide.

---

## Build from source

Requires the Go toolchain (see [go.mod](../../go.mod) for the required version).

```bash
git clone https://github.com/c0m4r/kula.git
cd kula
./addons/build.sh
```

For the full developer build matrix (cross-compilation, package builders), see
[Building & Toolchain](../dev/03-building.md).

---

## Where data is stored

By default Kula stores its tier files under `/var/lib/kula`. If it can't write there
(insufficient permissions), it falls back to `~/.kula`. You can override the location with
`storage.directory` in `config.yaml` or the `KULA_DIRECTORY` environment variable.

Next: [Quick Start](03-quick-start.md).
