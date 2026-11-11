# Landlock Sandbox

File: [`internal/sandbox/sandbox.go`](../../internal/sandbox/sandbox.go) (introduced in v0.4.0)

Kula confines itself at runtime using the **Linux Landlock LSM** via the
[`go-landlock`](https://github.com/landlock-lsm/go-landlock) library. After startup, the process
restricts its own filesystem and network access to what it needs. On a kernel with Landlock
ABI 10 or newer, a code-execution bug in `kula serve` can read only `/proc`, `/sys`, its config,
its storage directory and the few files listed below, write only its storage directory, and
reach only the TCP ports and Unix sockets of the applications you enabled, plus DNS when one of
them is named by a host name. It cannot send UDP anywhere else, signal processes outside the
sandbox or connect to their abstract Unix sockets. What Landlock does not confine is listed under
[Not confined](#not-confined); older kernels confine less (see the table below).

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

Kula requests every access right of the newest Landlock ABI go-landlock knows (`landlock.V11`,
the `requested` variable) with `BestEffort()`, so the kernel enforces the part it supports:

| Landlock ABI | Adds |
|--------------|------|
| 1 (Linux 5.13) | files: reading, writing, executing, creating and removing |
| 4 (Linux 6.7) | TCP bind and connect |
| 6 (Linux 6.12) | signals and abstract Unix sockets reaching processes outside the sandbox |
| 9 | connecting to pathname Unix sockets |
| 10 | UDP bind, connect and send |

The startup line names the kernel's ABI, what it confines and what it does not, for example:

```
Landlock sandbox enforced (ABI v10 confines files, TCP, signals, abstract Unix sockets, Unix sockets, UDP; paths: ...)
```

Without Landlock (a kernel before 5.13, or Landlock not enabled at boot) Kula logs that it skips
the sandbox and runs unconfined. A go-landlock upgrade that knows a newer ABI does not raise
`requested` by itself: a new ABI restricts more operations, so move only after running every
collector and listener under it (see [Tests](#tests)).

## Filesystem rules

| Path | Access |
|------|--------|
| `/proc`, `/sys` | read |
| config file | read |
| storage directory | read-write, and connecting to sockets there |
| `/etc/hosts`, `/etc/resolv.conf`, `/etc/nsswitch.conf` | read (name resolution) |
| `/run/utmp`, `/var/run/utmp`, `/run/systemd/sessions` | read (logged-in user count) |
| directory of `web.unix_socket` | create a socket, remove a file (when a Unix socket is configured) |
| `~/.postgresql`, `PGSSLROOTCERT`/`PGSSLCERT`/`PGSSLKEY` paths | read (PostgreSQL with `sslmode` other than `disable`) |
| `applications.mysql.sslrootcert` | read (MySQL with a CA file set; the file, not its directory) |
| directory `/etc/resolv.conf` links into (e.g. `/run/systemd/resolve`) | read (only when DNS is allowed) |

The read-write grant on the storage directory is what lets the storage engine, the custom-metrics
socket, the session file and the backup writer function under confinement. A file or directory
that does not exist at startup is skipped, and stays unreachable until Kula restarts; a grant on
a file follows that file, so one renamed over it (a CA file replaced rather than rewritten in
place) also needs a restart.

## Unix sockets

From ABI 9, connecting to a pathname Unix socket needs a grant. Kula grants only that right
("resolve unix"), and only for:

- the container runtime socket the collector will use, chosen from the collector's own candidate
  list (`collector.ContainerSocketPaths`: `applications.containers.socket_path`, Docker, rootful
  Podman, rootless Podman) the way the collector chooses — the first candidate that accepts a
  connection — so the two cannot drift apart;
- PostgreSQL with an absolute `host`: `<host>/.s.PGSQL.<port>` (with `port: 0`, the port of
  `PGPORT`, else 5432);
- MySQL with `port: 0`: the socket file named by `host`;
- `web.unix_socket`, so the server can tell a live listener from a stale socket before removing it;
- the storage directory, for the same check on the custom-metrics socket `kula.sock`.

A socket in a directory of its own (`/run/postgresql`, `/run/mysqld`, `/run/podman`,
`/var/lib/mysql`) is granted through that directory, so a server that recreates its socket on
restart stays reachable. A socket directly in a shared directory — `/run`, `/var/run`, `/tmp`,
a user's runtime directory `/run/user/<uid>`, a home directory, any world-writable directory, and
`/run/systemd` and `/run/dbus` — is granted as the socket file that exists at startup, since a
grant on the directory would open every socket below it (the D-Bus system bus, systemd's private
socket). That grant follows the file: if its server recreates it — `docker.sock` in `/run`, from a
`dockerd` that is not socket-activated — Kula reaches it again only after a restart. A socket
that does not exist yet at startup is logged with the same advice. A host that cannot hold a
socket at all (a path through a regular file) stops startup with an error naming the setting.

The sockets Kula itself listens on need no grant: Landlock restricts the connecting process, not
the listener. A PostgreSQL or MySQL host given as an abstract address (`@name`) leaves abstract
Unix sockets unconfined, and the startup line says so.

## TLS certificates

TLS verification — `https://` nginx/Apache2 status URLs, an `https://` Ollama URL, PostgreSQL
`verify-ca`/`verify-full`, MySQL `verify-full` without an `sslrootcert` — uses the system CA
roots. `crypto/x509` loads them once, on first use, and caches them. The bundle is usually a symlink into a distribution-specific tree
(`/etc/ca-certificates`, `/usr/share/ca-certificates`, `/etc/pki`), so rather than granting all
of those, `Enforce` loads the roots *before* confining the process whenever an enabled target
needs them (`needsSystemRoots`), and logs `tls:system-roots`. CA updates therefore take effect on
restart. Without this, every such connection failed certificate verification under the sandbox.

## Network rules

- **TCP bind** is allowed only on the configured web port (so the server can listen). With
  `web.unix_socket` set, no bind rule is added.
- **TCP connect** is allowed only to the ports of the **enabled** application collectors —
  conditionally added for nginx, Apache2, MySQL, PostgreSQL, and Ollama. Nginx, Apache2 and
  Ollama take the port from their URL, or the port the client dials without one (80, or 443 for
  `https`); MySQL uses its configured `port`, PostgreSQL its `port`, or with `port: 0` the ports
  of `PGPORT`, else 5432. Containers need no TCP rule because they are reached through the
  runtime socket instead.
- **UDP** is denied, except for DNS.
- **DNS** is allowed only when an enabled target — an nginx or Apache2 status URL, a PostgreSQL
  or MySQL `host`, `ollama.url` — is a host name Go's resolver cannot answer from `/etc/hosts`.
  An IP address needs none, and neither does a name `/etc/hosts` lists (`localhost`, usually)
  unless `/etc/nsswitch.conf` leaves `files` off its `hosts` line: Kula is built without cgo, so
  Go's own resolver reads `/etc/hosts` before DNS, or after a DNS lookup fails. DNS then means
  datagrams from an ephemeral UDP port to port 53, and TCP to port 53 for answers too large for a
  datagram; the systemd-resolved stub at `127.0.0.53` is reached the same way. Landlock rules name
  ports, not addresses, so this grant reaches port 53 of any host. The decision is made at
  startup: a target name added to or removed from `/etc/hosts` later needs a restart.

Because the rules are derived from your config at startup, enabling an application or changing
its address automatically updates the allowed set — no manual sandbox tweaking. Each applied rule
is logged (e.g. `nginx:connect-tcp/80`, `postgres:unix(/run/postgresql/)`,
`dns:udp+tcp/53(db.example.com)`).

## Signals and abstract Unix sockets

From ABI 6, the process cannot signal processes outside the sandbox, nor connect to an abstract
Unix socket one of them listens on. Kula needs neither (a database at an `@name` address aside,
above).

## Not confined

Landlock does not mediate these; the process's own credentials and the service manager do:

- **Multipath TCP and SCTP.** The TCP rules cover plain TCP sockets only. On Linux 7.2 with
  ABI 10, an MPTCP socket (`IPPROTO_MPTCP`) still connects to a port no rule grants, and an MPTCP
  connection to a server without MPTCP falls back to plain TCP — so a compromised process can
  still open TCP connections to any port. `sysctl net.mptcp.enabled=0` closes that for the whole
  host; systemd's `IPAddressDeny=`/`IPAddressAllow=` can limit the service's addresses. (Go's
  `net.Listen` itself uses MPTCP where the kernel enables it.)
- **ICMP echo sockets** where `net.ipv4.ping_group_range` admits Kula's group, and raw or packet
  sockets for a process with `CAP_NET_RAW` (the packaged unit drops every capability).
- **Netlink sockets.**
- Metadata operations Landlock does not cover (`stat`, `chmod`, `chown`, …), and file descriptors
  opened before enforcement, such as the inherited standard output and error.

## Adding a grant for a new collector

If you add an application collector that connects somewhere, extend the ruleset in
`addApplications` when the module is enabled: `rs.addURL(label, url)` for an HTTP URL (its TCP port
and, for a host name, DNS), `rs.addTCPConnect(label, port)` and `rs.addHostName(host)` for another
TCP client, `rs.addUnixSocket(key, label, path, true)` for a Unix socket, and
`rs.addReadOnly(key, path, false)` for a file it reads, such as a CA certificate. See
[Adding a Metric Type](14-adding-metrics.md#5-sandbox).

## Tests

[`sandbox_test.go`](../../internal/sandbox/sandbox_test.go) runs the real `Enforce` in a child
process and asserts, as far as the kernel's ABI enforces it:

- Writing outside the storage directory fails; writing inside it works.
- Executing outside the allowed paths fails.
- Dialing a TCP port no rule grants fails.
- Connecting to a Unix socket outside every grant fails, the D-Bus system bus included, while the
  granted ones connect — in a directory of their own and as a file in the shared temporary
  directory.
- UDP to any port fails, and to port 53 too unless a target is named by a DNS name.
- Signalling a process outside the sandbox, and connecting to its abstract socket, fail.
- utmp and systemd-logind sessions, when the host has them, stay readable.
- The MySQL `sslrootcert` stays readable, and a file beside it does not.
- With an `https://` target configured, the system CA roots are still available.

`TestSandboxConfinesSocketsUDPAndSignals` fails if a change re-opens UDP or arbitrary Unix
sockets. [`rules_test.go`](../../internal/sandbox/rules_test.go) pins the grants each PostgreSQL,
MySQL, socket and DNS setting produces. `TestServeUnderTheSandbox` in
[`cmd/kula`](../../cmd/kula/serve_sandbox_test.go) runs `kula serve` with every collector and
listener pointed at fakes outside its sandbox — the container runtime socket, PostgreSQL and
MySQL over TCP and a Unix socket, nginx and Apache2 by IP and by a hosts-file name, the
custom-metrics socket, the web server on a Unix socket and on TCP, `/metrics`, the Ollama proxy
at `localhost`, tier writes and the session file — and fails if one of them is not reached or
anything logs a denial.

Next: [Frontend](10-frontend.md).
