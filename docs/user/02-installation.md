# Installation

Kula ships as a single binary with everything embedded. You can upload it to a server and run
it without installing anything else. The examples below use **amd64 (x86_64)**; check the
[Releases page](https://github.com/c0m4r/kula/releases) for **ARM** and **RISC-V** packages.

> ⚠️ **Security note.** Never paste install commands blindly. Verifying a checksum confirms a
> download is intact, but it is no substitute for reviewing the code you are about to run.

Package versions and checksums below are placeholders — take current values from the
[latest release](https://github.com/c0m4r/kula/releases/latest) and its
`CHECKSUMS.sha256.txt`. The installer checksum is the hash of `install_v3.sh` on `main`. A
published installer never changes, so the checksum an older copy of these instructions pins
still matches the installer it names (`install_v2.sh`, or the original `install.sh`); a newer
installer gets a new name.

---

## Guided installer (recommended)

The installer auto-detects your distro and offers the best packaging option, verifying every
download against the release's `CHECKSUMS.sha256.txt`.

```bash
bash -c "$(curl -fsSL https://raw.githubusercontent.com/c0m4r/kula/refs/heads/main/addons/install_v3.sh)"
```

### Verifying the installer first

```bash
KULA_INSTALL=$(mktemp)
curl -o "${KULA_INSTALL}" -fsSL https://raw.githubusercontent.com/c0m4r/kula/refs/heads/main/addons/install_v3.sh
echo "a567aec0671e379f8a40204c848c9d5a85ba1331c6902f0b1e0688a2f210c0eb  ${KULA_INSTALL}" | sha256sum -c && bash "${KULA_INSTALL}"
rm -f "${KULA_INSTALL}"
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
echo "<sha256>  kula-<version>-amd64.tar.gz" | sha256sum -c && tar -xvf kula-<version>-amd64.tar.gz
cd kula
./kula
```

---

## Docker

Kula needs host PID and network namespaces plus read-only `/proc` to see the host. It needs no
capabilities, so drop them all.

**Ephemeral (no persistent storage):**

```bash
docker run --rm -it --name kula --pid host --network host \
  --cap-drop ALL --security-opt no-new-privileges:true \
  -v /proc:/proc:ro c0m4r/kula:latest
```

**With persistent storage:**

```bash
docker run -d --name kula --pid host --network host \
  --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  -v /proc:/proc:ro -v kula_data:/app/data c0m4r/kula:latest
docker logs -f kula
```

The image runs Kula as the unprivileged `kula` user, uid 100 and gid 101; these stay fixed, so a
data volume written by an earlier image keeps working. Root owns the binary and
`/app/config.yaml` (`root:kula 0640`), so Kula cannot rewrite them, and Kula writes only to
`/app/data`: with a volume there, `--read-only` costs nothing. (Without one, `/app/data` is part
of the image, so the ephemeral run above leaves the filesystem writable.)

- **Your own config:** mount it read-only over `/app/config.yaml`. It holds the password hash and
  tokens, so make it readable by the container's group only:
  `sudo chown root:101 config.yaml && sudo chmod 0640 config.yaml`.
- **A host directory for data** instead of a volume must be writable by uid 100:
  `sudo chown 100:101 /path/to/data && sudo chmod 0750 /path/to/data`.
- With `--pid host`, uid 100 is uid 100 on the host too: if a host account has that uid, Kula
  can signal its processes.

**Logged-in users:** Kula counts logins from the host's utmp or systemd-logind sessions, both
in the host's `/run`, which a container does not see. The count is then unknown: the dashboard
footer leaves it out, the TUI shows `—`, and `kula_system_logged_in_users` is omitted. To count
them, mount whichever of the two the host has, read-only:

```bash
docker run -d --name kula --pid host --network host \
  --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  -v /proc:/proc:ro -v kula_data:/app/data \
  --mount type=bind,source=/run/utmp,target=/run/utmp,readonly \
  --mount type=bind,source=/run/systemd/sessions,target=/run/systemd/sessions,readonly \
  c0m4r/kula:latest
```

`--mount` refuses a source the host does not have (`-v` would create it as a directory); drop
that line instead.

A `docker-compose.yml` is provided in [`addons/docker/`](../../addons/docker/docker-compose.yml);
it sets `read_only`, `cap_drop: [ALL]` and `no-new-privileges`, and mounts `./config.yaml`.
Images are published on [Docker Hub](https://hub.docker.com/r/c0m4r/kula) and GHCR.

---

## Debian / Ubuntu (.deb)

```bash
wget https://github.com/c0m4r/kula/releases/download/<version>/kula-<version>-amd64.deb
echo "<sha256>  kula-<version>-amd64.deb" | sha256sum -c && sudo dpkg -i kula-<version>-amd64.deb
journalctl -f -t kula
```

The package installs a systemd unit, then enables and starts Kula.

To upgrade, install the newer package the same way. A running Kula restarts on the new version,
a stopped one stays stopped, and the service stays enabled or disabled as you left it. If the
restart fails, the package says so and the upgrade still completes: `journalctl -u kula` shows
why.

---

## RHEL / Fedora / CentOS / Rocky / Alma (.rpm)

```bash
wget https://github.com/c0m4r/kula/releases/download/<version>/kula-<version>-x86_64.rpm
echo "<sha256>  kula-<version>-x86_64.rpm" | sha256sum -c && sudo rpm -i kula-<version>-x86_64.rpm
journalctl -f -t kula
```

The package installs a systemd unit, then enables and starts Kula. Upgrade with
`sudo rpm -U kula-<version>-x86_64.rpm`; the service is handled as for the
[`.deb`](#debian--ubuntu-deb): restarted only if it is running, its enabled state left alone.

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

The snap's daemon runs as **root**. snapd's confinement and the interfaces you connect are what
bound it, not an unprivileged user, and Kula's own Landlock sandbox does not apply inside a snap.
Connecting `kula:docker` hands that root daemon the Docker socket, which controls the host (see
[Application Monitoring](08-application-monitoring.md#containers-docker--podman)). The `.deb`,
`.rpm` and AUR packages run Kula as the unprivileged `kula` user.

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

## Where the config lives

The `.deb`, `.rpm` and AUR packages and the guided installer seed `/etc/kula/config.yaml` from
the example on first install. It holds the password hash, the Prometheus token and database
passwords, so it is `root:kula 0640` in a `root:kula 0750` directory: only root and the `kula`
group can read it, and the service cannot rewrite it. Edit it as root. Upgrades keep its
content and restore those modes. The snap keeps its config at
`/var/snap/kula/current/config.yaml`, readable by root only.

## Where data is stored

By default Kula stores its tier files under `/var/lib/kula`. If it can't write there
(insufficient permissions), it falls back to `~/.kula`. You can override the location with
`storage.directory` in `config.yaml` or the `KULA_DIRECTORY` environment variable.

Next: [Quick Start](03-quick-start.md).
