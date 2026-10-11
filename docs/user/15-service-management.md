# Service Management

Kula ships ready-made init scripts for the three common Linux service managers under
[`addons/init/`](../../addons/init/). The `.deb` and `.rpm` packages install the systemd unit
automatically; the steps below are for manual setup or non-systemd systems.

All of them run Kula as a dedicated unprivileged `kula:kula` user, with config at
`/etc/kula/config.yaml`, data at `/var/lib/kula`, and an optional Unix-socket runtime dir at
`/run/kula`.

## systemd

```bash
sudo cp addons/init/systemd/kula.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now kula
```

Follow the logs:

```bash
journalctl -f -t kula
# or
journalctl -u kula -f
```

The provided unit is hardened as **defense in depth on top of Kula's built-in Landlock
sandbox**:

- `Type=simple`, `Restart=on-failure` (5 s backoff).
- Runs as `User=kula`, `WorkingDirectory=/var/lib/kula`.
- `ProtectSystem=strict` with `ReadWritePaths=/var/lib/kula` and `ReadOnlyPaths=/etc/kula`.
- Drops all capabilities (`CapabilityBoundingSet=` / `AmbientCapabilities=`),
  `NoNewPrivileges=true`, `PrivateTmp`, `PrivateDevices`, `ProtectKernel*`, `RestrictNamespaces`,
  `LockPersonality`, `RemoveIPC`, `UMask=0077`.
- Address families limited to `AF_UNIX AF_INET AF_INET6 AF_NETLINK`.
- `RuntimeDirectory=kula` creates/cleans `/run/kula` for the optional Unix socket.
- `LimitNOFILE=65536`.

> Note: `/proc`-hiding (`ProtectProc`/`ProcSubset`) and syscall filtering are intentionally
> **omitted** — a monitoring daemon must read system-wide `/proc`, `/sys/fs/cgroup`, and
> runtime sockets, so over-restricting them would break collection. `ProtectClock` is omitted
> as well: its seccomp filter also denies the read-only `adjtimex()` call Kula uses to detect
> clock synchronization, which would report every clock as unsynchronized. Clock writes remain
> impossible because `CAP_SYS_TIME` is dropped.

## OpenRC

```bash
sudo cp addons/init/openrc/kula /etc/init.d/
sudo rc-update add kula default
sudo rc-service kula start
```

The script declares `need net` / `after firewall`, runs backgrounded as `kula:kula`, and
ensures `/var/lib/kula` and `/run/kula` exist with the right ownership via `start_pre`.

## runit

```bash
sudo cp -r addons/init/runit/kula /etc/sv/
sudo ln -s /etc/sv/kula /var/service/
```

The `run` script creates `/run/kula`, fixes ownership, and execs Kula as `kula:kula` with
`chpst`. A logging service is provided under `addons/init/runit/kula/log/`.

## Prerequisites (manual setup)

Before enabling any of these, ensure:

1. The binary is at `/usr/bin/kula`.
2. A `kula` system user/group exists:
   ```bash
   sudo useradd --system --home-dir /var/lib/kula --shell /usr/sbin/nologin kula
   ```
3. Config exists at `/etc/kula/config.yaml` (copy `config.example.yaml`).
4. Data dir exists: `sudo install -d -o kula -g kula /var/lib/kula`.

The distro packages handle all of this for you.

## Running in a container

For Docker/Podman, you don't need an init script — see the
[Docker section of Installation](02-installation.md#docker). Use `--pid host --network host -v
/proc:/proc:ro` so Kula can see the host.

Next: [Troubleshooting](16-troubleshooting.md).
