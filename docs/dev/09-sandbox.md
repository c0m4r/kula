# Landlock Sandbox

File: [`internal/sandbox/sandbox.go`](../../internal/sandbox/sandbox.go) (introduced in v0.4.0)

Kula confines itself at runtime using the **Linux Landlock LSM** via the
[`go-landlock`](https://github.com/landlock-lsm/go-landlock) library. After startup, the process
restricts its own filesystem and network access to the minimum it needs — so even a
hypothetical code-execution bug can't read arbitrary files or open arbitrary network
connections.

This is enforced **in-process**, independent of (and complementary to) the hardened systemd unit
([Service Management](../user/15-service-management.md)).

## When it's applied

`runServe` calls `sandbox.Enforce(configPath, storageDir, webCfg, appsCfg, ollamaCfg)` early,
*before* the collection loop starts:

```go
if err := sandbox.Enforce(configPath, cfg.Storage.Directory, cfg.Web,
                          cfg.Applications, cfg.Ollama); err != nil {
    log.Printf("Warning: Landlock sandbox not enforced: %v", err)
}
```

It is **non-fatal**: on unsupported kernels the warning is logged and Kula runs unconfined.

## Kernel requirements & graceful degradation

- Requires **kernel 5.13+** with Landlock enabled.
- Kula checks the Landlock **ABI version** at startup and uses `BestEffort()` so it degrades
  gracefully on older kernels (applying whatever subset is available, or nothing).
- Network confinement additionally needs **ABI v4+ (kernel 6.7+)**; below that only the
  filesystem rules apply and the enforcement log says network protection is unsupported.

## Filesystem rules

| Path | Access |
|------|--------|
| `/proc` | read-only |
| `/sys` | read-only |
| config file | read-only |
| storage directory | read-write |
| `/etc/hosts`, `/etc/resolv.conf`, `/etc/nsswitch.conf` | read-only (for DNS) |
| `/run/utmp`, `/var/run/utmp`, `/run/systemd/sessions` | read-only (logged-in user count) |
| parent directory of `web.unix_socket` | read-write (when a Unix socket is configured) |
| container runtime socket the collector will use (`applications.containers.socket_path`, else the first reachable of `collector.ContainerSocketPaths`) | read-write (when containers are enabled) |
| `applications.postgres.host` | read-write (Unix socket mode) |
| `applications.mysql.host` | read-write (Unix socket file mode) |
| `~/.postgresql`, `PGSSLROOTCERT`/`PGSSLCERT`/`PGSSLKEY` paths | read-only (PostgreSQL with `sslmode` other than `disable`) |

The read-write grant on the storage directory is what lets the storage engine, the custom-metrics
socket, and the backup writer function under confinement.

The container socket is chosen from the collector's own candidate list
(`collector.ContainerSocketPaths`: configured path, Docker, rootful Podman, rootless Podman of the
running user, `/run/user/1000`), picking what the collector will pick — the first candidate that
accepts a connection — so the two cannot drift apart. Note that the filesystem rights Kula
requests (Landlock ABI ≤ 5) do not mediate `connect(2)` on a pathname Unix socket; the socket
grants (containers, PostgreSQL and MySQL socket modes) document intent and would apply to a
ruleset that handles such connects, but today a socket's own permissions decide.

## TLS certificates

TLS verification — `https://` nginx/Apache2 status URLs, an `https://` Ollama URL, PostgreSQL
`verify-ca`/`verify-full` — uses the system CA roots. `crypto/x509` loads them once, on first
use, and caches them. The bundle is usually a symlink into a distribution-specific tree
(`/etc/ca-certificates`, `/usr/share/ca-certificates`, `/etc/pki`), so rather than granting all
of those, `Enforce` loads the roots *before* confining the process whenever an enabled target
needs them (`needsSystemRoots`), and logs `tls:system-roots`. CA updates therefore take effect on
restart. Without this, every such connection failed certificate verification under the sandbox.

## Network rules

Network rules require Landlock **ABI v4+** (Linux 6.7+); on older kernels the filesystem rules
still apply, but no bind or connect rule does.

- **TCP bind** is allowed only on the configured web port (so the server can listen). With
  `web.unix_socket` set, no bind rule is added.
- **TCP connect** is allowed only to the ports of the **enabled** application collectors —
  conditionally added for nginx, Apache2, MySQL, PostgreSQL, and Ollama. Nginx, Apache2 and
  Ollama take the port from their URL, or the port the client dials without one (80, or 443 for
  `https`); PostgreSQL and MySQL use their configured `port`. Containers need no TCP rule
  because they are reached through the runtime socket instead.

Because the connect rules are derived from your config at startup, enabling an application or
changing its port automatically updates the allowed set — no manual sandbox tweaking. Each
applied rule is logged (e.g. `foo:connect-tcp/9000`).

## Adding a connect rule for a new collector

If you add an application collector that makes outbound HTTP/DB connections, you must extend the
sandbox so it can reach that port. The pattern (parse the port from the configured URL, append a
`landlock.ConnectTCP(port)` rule when the module is enabled) is shown step-by-step in
[Adding a Metric Type](14-adding-metrics.md#5-sandbox).

## Tests

[`sandbox_test.go`](../../internal/sandbox/sandbox_test.go) asserts that, once enforced:

- Writing outside the storage directory fails.
- Executing outside the allowed paths fails.
- Dialing an external (non-allowed) network address fails.
- utmp and systemd-logind sessions, when the host has them, stay readable.
- With an `https://` target configured, the system CA roots are still available.

These are the negative tests proving the confinement actually holds.

Next: [Frontend](10-frontend.md).
