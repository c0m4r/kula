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

**Metrics:** busy/idle workers, open slots, scoreboard states, requests/s and kB/s,
bytes/request, CPU load, total accesses and kbytes, uptime. Requests/s and kB/s are current
rates, from the change in Apache's totals since the previous poll; they need
`ExtendedStatus On`, and a restart or a failed poll reports no rate for that poll rather than
an inflated one. Bytes/request and CPU load are Apache's own averages since it started.
mod_status also reports requests/s and bytes/s averaged since start; the dashboard and the AI
assistant no longer show those, and their Prometheus series are deprecated (see
[Prometheus](11-prometheus.md)).

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
  Docker, Podman and Podman Quadlet cgroup layouts. A cgroup counts as a container only when it
  is named after a full container ID (64 hex digits) and owned by root or by the user Kula runs
  as: systemd gives every logged-in user a cgroup subtree of their own, where they can create
  any directory, so another user's rootless containers are not counted, and no user can make
  up containers to fill your history.
- **`none`** — neither a runtime socket nor a cgroups v2 tree was found; container monitoring
  stays disabled.

The `socket_path` is auto-detected: Docker (`/var/run/docker.sock`, `/run/docker.sock`), rootful
Podman (`/run/podman/podman.sock`), then the rootless Podman socket of the user Kula runs as
(`$XDG_RUNTIME_DIR/podman/podman.sock`, `/run/user/<uid>/podman/podman.sock`). Another user's
rootless socket is never picked automatically, even for a Kula running as root: that user's
Podman would decide which containers and processes Kula reads. To monitor it, run Kula as that
user, or, as root, set `socket_path` to it explicitly. Set it explicitly for any other location
too. Leave the `containers` filter empty to monitor all running containers.

Kula collects only containers whose runtime reports a full container ID (64 hex digits), and at
most 1,024 per sample: past that, the ones whose names sort first. A name longer than 255 bytes
is shortened to its start, `…` and a short hash.

> **A container runtime socket is root on the host.** Whoever can use the Docker socket, or
> rootful Podman's, can start a privileged container that mounts the host's filesystem. Adding
> the `kula` user to the `docker` group, or running Kula as root, to get container names
> therefore makes Kula root-equivalent: anyone who takes over Kula takes over the host. The
> packages do not do it for you. If you can do without names, leave Kula without the socket:
> `cgroups` mode needs no privilege and gives the same per-container metrics. A rootless
> Podman socket is worth only its own user's privileges.

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

As with `psql`, settings the config leaves out are taken from the standard `PG*` environment
variables of the Kula process (`PGSSLROOTCERT`, `PGPASSWORD`, `PGOPTIONS`, …), and a missing
password from `~/.pgpass`. The sandbox lets Kula read the `PGSSL*` files and `~/.postgresql`,
but not `~/.pgpass`: under it, set the password here or in `KULA_POSTGRES_PASSWORD`.

**Metrics:** connections (active/idle/idle-in-transaction/waiting/max), transactions
committed/rolled back per second, tuples (returned/fetched/inserted/updated/deleted),
buffer cache hit %, blocks read/hit, checkpoint/backend buffers, deadlocks, dead/live tuples,
autovacuum count, database size, replication lag (bytes & seconds), replicas connected, and
recovery state.

On a standby, replication lag in seconds is the time since the last replayed commit while
there is WAL left to replay, and 0 once the standby has replayed everything it received from a
connected primary, so a quiet primary no longer reads as a growing lag. When the standby loses
its primary, the time grows again as soon as its WAL receiver gives up
(`wal_receiver_timeout`, 60 s by default). WAL the primary has not sent yet is invisible to a
standby, so a slow link can read as caught up. The `pg_monitor` role lets Kula see whether the
receiver is streaming; without it, Kula only sees that a receiver is running.

Dead/live tuples, autovacuum count and database size are read once a minute and repeated in
between: both walk every table of the database, which costs the server tens of milliseconds
per poll once it holds thousands of tables. A read that fails repeats the previous values for
another minute, and two failures in a row show zero. Everything else is read every collection
interval.

> Prefer the `KULA_POSTGRES_PASSWORD` environment variable over putting the password in the
> file. Kula escapes it safely for the libpq connection string.

**TLS:** `sslmode` takes the libpq values (`disable`, `allow`, `prefer`, `require`, `verify-ca`,
`verify-full`). `verify-ca` and `verify-full` check the server certificate against the system
CA certificates, or against `~/.postgresql/root.crt` (or `PGSSLROOTCERT`) for a private CA; a
client certificate is read from `~/.postgresql/postgresql.crt` and `.key` (or `PGSSLCERT` and
`PGSSLKEY`). The sandbox allows reading exactly those files. Over a network use `verify-full`:
see [Securing the database connection](#securing-the-database-connection).

**PgBouncer:** since 0.22.0, Kula sends its session limits (`statement_timeout`, `lock_timeout`,
`idle_in_transaction_session_timeout`, `tcp_keepalives_idle`, `tcp_keepalives_interval`,
`tcp_keepalives_count`; see [Database connections](#database-connections)) as startup
parameters. PgBouncer refuses a connection carrying a parameter it does not track
(`unsupported startup parameter`) unless `pgbouncer.ini` lists it:

```ini
ignore_startup_parameters = statement_timeout,lock_timeout,idle_in_transaction_session_timeout,tcp_keepalives_idle,tcp_keepalives_interval,tcp_keepalives_count
```

PgBouncer then drops them, so those server-side limits do not apply to Kula's session. Point
Kula at PostgreSQL directly where you can.

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
    sslmode: "disable"
    # sslrootcert: "/etc/kula/mysql-ca.pem"
```

**TLS:** `sslmode` takes the PostgreSQL names for MySQL's own `--ssl-mode` values, and applies
to TCP (a Unix socket needs no TLS, and Kula refuses the combination):

- `disable` (default): no TLS.
- `require` (`REQUIRED`): encrypted, but the certificate is not checked. With `sslrootcert` set,
  it is checked as for `verify-ca`.
- `verify-ca` (`VERIFY_CA`): the certificate must be issued by the CA in `sslrootcert`, which
  this mode requires; the name in it is not checked.
- `verify-full` (`VERIFY_IDENTITY`): the certificate must be issued by the CA in `sslrootcert`,
  or by one of the system CAs when none is set, and name `host`.

A server that does not offer TLS is refused, never used unencrypted. `sslrootcert` is a PEM file
read for each connection, so a CA file rewritten in place needs no restart. The sandbox allows
reading that one file as it was at startup: one replaced by a new file (renamed over it, or a
symlink pointed elsewhere) is readable again after a restart.

**Metrics:** threads (connected/running/cached), max connections, queries/select/insert/
update/delete/slow per second, InnoDB buffer-pool reads/s and hit %, row-lock waits,
table-lock waits, plus replication state (IO/SQL running, seconds-behind, last IO/SQL errno,
IO state, replicas connected).

> **Caveat on `Seconds_Behind_Source`:** this is the SQL thread's view of how far behind it is
> on what it currently sees, *not* the true lag from the primary.

## Securing the database connection

Kula logs in with the configured password, and over TCP it cannot tell the database from
anything else that answers on `host:port` unless it verifies the server's certificate. Whatever
answers gets the password: with `sslmode: disable`, PostgreSQL may ask for it in clear text, and
MySQL's `caching_sha2_password` encrypts it to a key the server itself supplies. That can be a
local user who binds the port while the database is down or not yet started, anyone on the
network path to a remote database, or a compromised server. (A reply crafted to crash the
driver only fails a poll, see below; the password is gone all the same.)

- **Database on the same host: connect through its Unix socket.** The socket lives in a
  directory only the database's own account can write (`/var/run/postgresql`,
  `/var/run/mysqld`), so no other local user can put one of their own in its place; a socket in
  a world-writable directory such as `/tmp` gives no such guarantee. PostgreSQL:
  `host: "/var/run/postgresql"`, `port: 0`. MySQL: `host: "/var/run/mysqld/mysqld.sock"`,
  `port: 0`.
- **Database across a network: verify its certificate** with `sslmode: verify-full` (both
  databases), and `host` set to the name in the certificate. For a private CA give its
  certificate in `sslrootcert` (MySQL) or `~/.postgresql/root.crt` / `PGSSLROOTCERT`
  (PostgreSQL). `require` only encrypts: it keeps out an eavesdropper, not whoever answers in
  the server's place, who then reads the password. `verify-ca` accepts any certificate its CA
  issued, so use it only with a CA that certifies nothing but your database servers.
- Give the monitoring user only the read-only grants above, and a password used nowhere else.

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
- **A malformed reply fails one poll.** A reply that crashes the database driver (from a
  broken server, or from whatever answers on its port) fails that poll only: Kula logs it once,
  closes the connection and backs off as above, while everything else keeps running.
- **Clean shutdown.** Stopping Kula closes its sessions.

No extra privilege is needed: a database user may always end its own sessions.

---

## Custom metrics

For anything not built in, Kula accepts your own metrics over a Unix socket. See
[Custom Metrics](09-custom-metrics.md).

Next: [Custom Metrics](09-custom-metrics.md).
