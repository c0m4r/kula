# Service Management

Kula ships ready-made init scripts for the three common Linux service managers under
[`addons/init/`](../../addons/init/). The `.deb` and `.rpm` packages install the systemd unit
automatically; the steps below are for manual setup or non-systemd systems.

All of them run Kula as a dedicated unprivileged `kula:kula` user, with config at
`/etc/kula/config.yaml`, data at `/var/lib/kula`, and an optional Unix-socket runtime dir at
`/run/kula`. The config holds the password hash, the Prometheus token and database passwords,
so root owns it: `/etc/kula` is `root:kula 0750` and `config.yaml` `root:kula 0640`. The
service reads it through its group and cannot rewrite it; `/var/lib/kula` is `kula:kula 0750`.

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
- `MemoryMax=1G` (see below).

### Memory limit

`MemoryMax=1G` is a backstop, not a budget: should Kula's memory ever run away, the kernel
kills Kula, and `Restart=on-failure` starts it again, instead of picking another process on
the host. Kula answers at most two history requests at a time, so its memory no longer grows
with the number of requests: with three days of history, a flood of requests for all of it at
full width peaked at about 340 MB (10 at once) and 470 MB (30 at once) on a host with a few
containers. Samples grow with every container, disk and interface, and the same flood peaked
at about 1 GB with 40 containers.

On a host with dozens of containers, disks or interfaces, raise the limit with a drop-in rather
than by editing the unit, so a package upgrade keeps it:

```bash
sudo systemctl edit kula
```

```ini
[Service]
MemoryMax=2G
```

`MemoryMax=infinity` removes the limit. `systemctl status kula` shows Kula's memory use
against it on the `Memory:` line.

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

Unlike the systemd unit, the OpenRC script adds no confinement beyond the `kula` user: no
read-only system, private `/tmp`, dropped capability bounding set or memory limit. Kula's own
Landlock sandbox still applies, on a kernel that has Landlock enabled; without it, the user's
permissions are all that bound Kula.

## runit

```bash
sudo cp -r addons/init/runit/kula /etc/sv/
sudo ln -s /etc/sv/kula /var/service/
```

The `run` script creates `/run/kula`, fixes ownership, and execs Kula as `kula:kula` with
`chpst`. A logging service is provided under `addons/init/runit/kula/log/`; it writes to
`/var/log/kula`, which it creates.

Like OpenRC, runit adds no confinement beyond the `kula` user; only Kula's Landlock sandbox,
where the kernel has it, goes further.

## Prerequisites (manual setup)

Before enabling any of these, ensure:

1. The binary is at `/usr/bin/kula`.
2. A `kula` system user/group exists:
   ```bash
   sudo useradd --system --home-dir /var/lib/kula --shell /usr/sbin/nologin kula
   ```
3. Config exists at `/etc/kula/config.yaml`, readable by the `kula` group only:
   ```bash
   sudo install -d -m 0750 -o root -g kula /etc/kula
   sudo install -m 0640 -o root -g kula config.example.yaml /etc/kula/config.yaml
   ```
4. Data dir exists: `sudo install -d -m 0750 -o kula -g kula /var/lib/kula`.

The distro packages handle all of this for you.

## Running in a container

For Docker/Podman, you don't need an init script — see the
[Docker section of Installation](02-installation.md#docker). Use `--pid host --network host -v
/proc:/proc:ro` so Kula can see the host.

Next: [Troubleshooting](16-troubleshooting.md).
