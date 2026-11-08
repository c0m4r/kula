# Application Monitoring

Beyond system metrics, Kula can monitor several services. Each module appears under the
**Applications** section of the dashboard and is independently enabled. All live under the
`applications:` block in `config.yaml`.

> **Sandbox note.** Kula's Landlock sandbox only permits outbound connections to the ports of
> the applications you enable. If you change an application's port, Kula automatically allows
> it because the rule is derived from your config at startup.

---

## Nginx

Uses the **stub_status** module.

**Prerequisites:** nginx compiled with `--with-http_stub_status_module`, and a status
location:

```nginx
location /status {
    stub_status;
    allow 127.0.0.1;
    deny all;
}
```

**Config:**

```yaml
applications:
  nginx:
    enabled: true
    status_url: "http://localhost/status"
```

**Metrics:** active connections, reading/writing/waiting, accepts/handled/requests per second
(and cumulative totals). Counter resets on nginx restart are handled gracefully: the poll that
sees the restart reports no rates, and a failed poll never inflates the next one.

nginx and Apache2 status pages are polled in the background, so a server that stops answering
cannot delay the rest of the sample. A request may take up to twice the collection interval
(at least 2 s); a page that has not answered for three intervals (at least 3 s) reads as
missing. Redirects are not followed. A failure is logged when it starts or changes, and again
when the page answers.

---

## Apache2

Uses the **mod_status** module.

**Prerequisites:** `a2enmod status` and a status location (`ExtendedStatus On` recommended for
full scoreboard detail):

```apache
<Location /server-status>
    SetHandler server-status
    Require local
</Location>
```

**Config:**

```yaml
applications:
  apache2:
    enabled: true
    status_url: "http://localhost/server-status?auto"
```

**Metrics:** busy/idle workers, open slots, scoreboard states, requests/s, bytes/s,
bytes/request, CPU load, total accesses and kbytes, uptime. Accesses/s and kB/s need
`ExtendedStatus On`; a restart or a failed poll reports no rate for that poll rather than an
inflated one.

---

## Containers (Docker / Podman)

```yaml
applications:
  containers:
    enabled: true
    # socket_path: "/var/run/docker.sock"
    # containers: ["my-app", "postgres-db"]   # filter by name or ID prefix
```

**Discovery modes** (logged at startup):

- **`socket`** — uses the container runtime API (Docker/Podman socket) for discovery, plus
  cgroups v2 for metrics; the cgroup is resolved through the container PID. Gives you container
  **names**, and handles plain Docker/Podman scopes as well as Podman Quadlet units, rootless
  services and custom slices.
- **`cgroups`** — fallback when no socket is reachable; metrics only, no name mapping. Scans
  Docker, Podman and Podman Quadlet cgroup layouts.
- **`none`** — neither a runtime socket nor a cgroups v2 tree was found; container monitoring
  stays disabled.

The `socket_path` is auto-detected: Docker (`/var/run/docker.sock`, `/run/docker.sock`), rootful
Podman (`/run/podman/podman.sock`), then the rootless Podman socket of the user Kula runs as
(`$XDG_RUNTIME_DIR/podman/podman.sock`, `/run/user/<uid>/podman/podman.sock`) and, for a Kula
running as root, `/run/user/1000/podman/podman.sock`. Set it explicitly for any other location —
another user's rootless socket is only reachable by running Kula as that user. Leave the
`containers` filter empty to monitor all running containers.

**Metrics:** per-container CPU%, memory used/limit/%, network RX/TX bytes/s, disk read/write
bytes/s. Each metric type gets its own multi-series chart (CPU, memory, network RX, network TX,
disk read, disk write) with one series per container, plus an **Applications** filter in the
section header that shows or hides individual containers (the selection is remembered per
browser). The memory chart plots usage only; the used/limit summary is shown in the card
subtitle.

---

## PostgreSQL

Connects via the `lib/pq` driver over TCP or a Unix socket (for a Unix socket, set `host` to
the socket directory and `port` to `0`).

**Create a read-only monitoring role:**

```sql
sudo -u postgres psql
CREATE ROLE kula_monitor WITH LOGIN PASSWORD 'changeme';
GRANT pg_monitor TO kula_monitor;
-- pg_monitor grants read access to pg_stat_*, pg_database_size, etc.
-- No table-level permissions needed.
```

**`pg_hba.conf`** (add before any reject rules):

```
host    postgres    kula_monitor    127.0.0.1/32    scram-sha-256
```

Then `sudo systemctl reload postgresql`.

**Config:**

```yaml
applications:
  postgres:
    enabled: true
    host: "localhost"
    port: 5432
    user: "kula_monitor"
    password: ""        # or set KULA_POSTGRES_PASSWORD (escaped safely)
    dbname: "postgres"
    sslmode: "disable"
```

**Metrics:** connections (active/idle/idle-in-transaction/waiting/max), transactions
committed/rolled back per second, tuples (returned/fetched/inserted/updated/deleted),
buffer cache hit %, blocks read/hit, checkpoint/backend buffers, deadlocks, dead/live tuples,
autovacuum count, database size, replication lag (bytes & seconds), replicas connected, and
recovery state.

> Prefer the `KULA_POSTGRES_PASSWORD` environment variable over putting the password in the
> file. Kula escapes it safely for the libpq connection string.

**TLS:** `sslmode` takes the libpq values (`disable`, `allow`, `prefer`, `require`, `verify-ca`,
`verify-full`). `verify-ca` and `verify-full` check the server certificate against the system
CA certificates, or against `~/.postgresql/root.crt` (or `PGSSLROOTCERT`) for a private CA; a
client certificate is read from `~/.postgresql/postgresql.crt` and `.key` (or `PGSSLCERT` and
`PGSSLKEY`). The sandbox allows reading exactly those files.

---

## MySQL / MariaDB

Connects via `go-sql-driver/mysql` over TCP or a Unix socket (set `host` to the socket path
and `port` to `0` for a socket).

**Create a read-only monitoring user:**

```sql
sudo mysql
CREATE USER 'kula_monitor'@'127.0.0.1' IDENTIFIED BY 'changeme';
GRANT PROCESS, REPLICATION CLIENT ON *.* TO 'kula_monitor'@'127.0.0.1';
FLUSH PRIVILEGES;
```

For full **replication** monitoring, also grant:

```sql
GRANT SLAVE MONITOR, BINLOG MONITOR, REPLICATION MASTER ADMIN
  ON *.* TO 'kula_monitor'@'127.0.0.1';
```

- `SLAVE MONITOR` is needed for `SHOW REPLICA STATUS` (IO/SQL thread state, seconds-behind,
  last errno, IO state).
- `REPLICATION MASTER ADMIN` is needed for `SHOW SLAVE HOSTS` / `SHOW REPLICAS` (the "replicas
  connected" gauge).

Without these, replication metrics quietly degrade to zero/sentinel values rather than
breaking the whole collector.

**Config:**

```yaml
applications:
  mysql:
    enabled: true
    host: "127.0.0.1"
    port: 3306
    user: "kula_monitor"
    password: ""        # or set KULA_MYSQL_PASSWORD
    dbname: ""   # leave empty; global status is server-wide
```

**Metrics:** threads (connected/running/cached), max connections, queries/select/insert/
update/delete/slow per second, InnoDB buffer-pool reads/s and hit %, row-lock waits,
table-lock waits, plus replication state (IO/SQL running, seconds-behind, last IO/SQL errno,
IO state, replicas connected).

> **Caveat on `Seconds_Behind_Source`:** this is the SQL thread's view of how far behind it is
> on what it currently sees, *not* the true lag from the primary.

## Database connections

Kula is built never to be the reason a PostgreSQL or MySQL server runs out of connections:

- **One session per database, per Kula process.** `kula serve` holds exactly one connection to
  each monitored server and reuses it for every poll; each running `kula tui` holds one more.
  The session is labelled: `application_name = kula` in PostgreSQL's `pg_stat_activity`, and
  the `program_name: kula` connection attribute in MySQL/MariaDB.
- **Slow polls wait instead of piling up.** Polls run in the background, one at a time. A poll
  that takes longer than the collection interval delays the next one; it never runs beside it.
- **The server ends runaway work by itself.** Kula's session carries its own limits:
  statements stop after 5 s (`statement_timeout`; MariaDB `max_statement_time`; MySQL
  `max_execution_time` for SELECTs), lock waits after 2 s (`lock_timeout` /
  `lock_wait_timeout`), and a session nothing is sent on for a minute is closed
  (PostgreSQL 14+ `idle_session_timeout` plus TCP keepalives; MySQL `wait_timeout`). If the
  Kula host vanishes (network loss, crash), its session is gone within about a minute rather
  than the hours the server defaults allow. The limits scale up with longer collection
  intervals, up to a one-minute poll limit.
- **No session is left behind.** If a poll cannot finish within 10 s (twice the collection
  interval when that is longer, at most one minute), Kula closes that connection, even when
  the server has frozen and stopped answering, and, from its next connection, ends the old
  session (`pg_terminate_backend` /
  `KILL CONNECTION` — its own session only, identified so that no other client's can match).
  While that old session is still alive, Kula does not poll beside it. So even during an
  incident there are at most two Kula sessions per server, one of them on its way out.
- **Failures back off.** A server that is down or refusing connections is retried after one
  collection interval (1 s by default), then after twice as long each time, up to 5 minutes,
  not every interval.
- **Clean shutdown.** Stopping Kula closes its sessions.

No extra privilege is needed: a database user may always end its own sessions.

---

## Custom metrics

For anything not built in, Kula accepts your own metrics over a Unix socket. See
[Custom Metrics](09-custom-metrics.md).

Next: [Custom Metrics](09-custom-metrics.md).
