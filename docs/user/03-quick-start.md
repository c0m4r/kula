# Quick Start

## Run it

```bash
./kula
```

That's it. The dashboard is now available at:

```
http://localhost:27960
```

(Earlier versions of Kula used port `8080`.)

Kula starts the `serve` command by default — it launches the collector, the storage engine,
the Landlock sandbox, and the web server, and begins writing one sample per second.

## Change the listen address and port

Either edit `config.yaml` (see [Configuration](04-configuration.md)) or use environment
variables:

```bash
export KULA_LISTEN="127.0.0.1"
export KULA_PORT="27960"
./kula
```

## Try the terminal UI

```bash
./kula tui
```

A self-contained terminal dashboard with tabs for Overview, CPU, Memory, Network, Storage,
Processes, and GPU. See [Terminal UI](06-tui.md).

## Inspect the storage

```bash
./kula inspect
```

Prints per-tier statistics — record counts, time ranges, how full each ring-buffer is, and the
estimated coverage and time until each tier fills.

## List your disks

```bash
./kula disks
```

Prints every disk and partition Kula can monitor with its persistent ID. Copy an ID into
`collection.devices` to keep monitoring the same drive across reboots and kernel-name changes.
See [Configuration](04-configuration.md).

## Check it's healthy

Kula exposes lightweight liveness endpoints:

```bash
curl http://localhost:27960/health
curl http://localhost:27960/status
```

Both return:

```
200 OK
kula is healthy
```

## Enable authentication (optional)

Generate a password hash and add it to your config:

```bash
./kula hash-password
```

Copy the printed `password_hash` and `password_salt` into the `web.auth` section of
`config.yaml` and set `enabled: true`. See [Authentication](07-authentication.md).

## Run it as a service

Init files for systemd, OpenRC, and runit live under `addons/init/`. See
[Service Management](15-service-management.md). The `.deb` and `.rpm` packages set this up
automatically.

---

That's the whole loop: run, view, optionally secure. Continue to
[Configuration](04-configuration.md) to tune Kula to your environment.
