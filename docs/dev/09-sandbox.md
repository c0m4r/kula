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
| parent directory of `web.unix_socket` | read-write (when a Unix socket is configured) |
| container runtime socket (`applications.containers.socket_path`, auto-detected when empty) | read-write (when containers are enabled) |
| `applications.postgres.host` | read-write (Unix socket mode) |
| `applications.mysql.host` | read-write (Unix socket file mode) |

The read-write grant on the storage directory is what lets the storage engine, the custom-metrics
socket, and the backup writer function under confinement.

## Network rules

Network rules require Landlock **ABI v4+** (Linux 6.7+); on older kernels the filesystem rules
still apply, but no bind or connect rule does.

- **TCP bind** is allowed only on the configured web port (so the server can listen). With
  `web.unix_socket` set, no bind rule is added.
- **TCP connect** is allowed only to the ports of the **enabled** application collectors —
  conditionally added for nginx, Apache2, MySQL, PostgreSQL, and Ollama. Nginx and Apache2 parse
  the port from their status URL (defaulting to 80, or 443 for `https`); PostgreSQL and MySQL
  use their configured `port`; Ollama parses its URL and defaults to 11434. Containers need no
  TCP rule because they are reached through the runtime socket instead.

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

These are the negative tests proving the confinement actually holds.

Next: [Frontend](10-frontend.md).
