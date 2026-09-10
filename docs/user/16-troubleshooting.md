# Troubleshooting

Common problems and how to resolve them. For anything not covered here, check the project
[wiki](https://github.com/c0m4r/kula/wiki) and [issues](https://github.com/c0m4r/kula/issues).

## Kula won't write data / falls back to `~/.kula`

The default storage directory is `/var/lib/kula`. If Kula can't write there (permissions), it
**falls back to `~/.kula`** and logs it. Either:

- Run as a user who owns `/var/lib/kula` (the distro packages create the `kula` user), or
- Set `storage.directory` (or `KULA_DIRECTORY`) to a writable path.

Check current storage health:

```bash
kula inspect
```

## "Landlock sandbox not enforced" warning

```
Warning: Landlock sandbox not enforced: ...
```

The Landlock LSM requires **kernel 5.13+** (and Landlock enabled in the kernel). On older
kernels Kula degrades gracefully (best-effort) and keeps running **without** the sandbox — it
is a hardening layer, not a requirement. To get full confinement, run on a newer kernel. See
[Landlock Sandbox](../dev/09-sandbox.md).

## Dashboard loads but shows no live data

- Check the WebSocket isn't blocked. Behind a reverse proxy, make sure you forward
  `Upgrade`/`Connection` headers (see [Reverse Proxy & TLS](13-reverse-proxy.md)).
- If you set `web.security.origin_validation: true` (default) and access Kula from a different
  origin, the WebSocket upgrade is rejected (CSWSH protection). Add the origin to
  `allowed_origins`.
- Per-IP WebSocket connections are capped (default 5). Close stale tabs or raise
  `max_websocket_conns_per_ip`.

## Can't log in / locked out

- Login is rate-limited over a **5-minute window**: **5 attempts per username *and* IP**,
  and **15 attempts per IP** overall. Wait ~5 minutes after repeated failures.
- Regenerate the hash with `./kula hash-password` if you're unsure of the password, and make
  sure both `password_hash` **and** `password_salt` are pasted.
- Make sure the `web.auth.argon2` parameters at hash-generation time match those in the
  running config.

## Application metrics show zeros or are missing

- **nginx/Apache2:** verify the status URL is reachable from the Kula host
  (`curl http://localhost/status`). The sandbox only allows the configured port.
- **PostgreSQL/MySQL:** confirm the monitoring user has the required grants (see
  [Application Monitoring](08-application-monitoring.md)). Replication metrics need extra
  grants and otherwise degrade silently to zero.
- **Containers:** if you see metrics but no names, Kula is in `cgroups` fallback mode — give
  it access to the Docker/Podman socket (`socket_path`) for name mapping.
- Enable `web.logging.level: debug` to see collector auto-discovery details.

## GPU not detected (especially NVIDIA)

NVIDIA monitoring may require extra setup (Kula reads from an `nvidia.log` CSV file produced by
a helper). See [`scripts/nvidia-exporter.sh`](../../scripts/nvidia-exporter.sh) and the wiki
[GPU monitoring page](https://github.com/c0m4r/kula/wiki/GPU-monitoring). AMD/Intel GPUs are
read directly from sysfs and usually work out of the box.

## High memory usage

Very coarse Tier 1/Tier 2 resolutions force Kula to buffer many samples in memory before each
aggregation flush. Stick close to the default tier resolutions (`1s` / `1m` / `5m`). The tier
validator caps the ratio between adjacent tiers (300:1) for this reason.

## Wrong / merged mount points in containers

Use `collection.mounts_detection` (or `KULA_MOUNTS_DETECTION`):

- `host` — only host mounts (`/proc/1/mounts`).
- `self` — only the container's own mounts (`/proc/self/mounts`).
- `auto` — merge both (default).

## Port already in use

Change `web.port` / `KULA_PORT`, or switch to a Unix socket with `web.unix_socket`.

## Diagnostics checklist

1. `kula --version` — confirm the version.
2. `kula inspect` — confirm storage is healthy and growing.
3. `curl localhost:27960/health` — confirm the server is up.
4. Raise `web.logging.level` to `debug` and watch the logs.
5. Run [`kula-scan`](../dev/13-kula-scan.md) against the instance to verify the security
   posture of your deployment.
