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
(and cumulative totals). Counter resets on nginx restart are handled gracefully.

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
bytes/request, CPU load, total accesses and kbytes, uptime.

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

The `socket_path` is auto-detected; set it explicitly for non-standard locations or rootless
Podman. Leave the `containers` filter empty to monitor all running containers.

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

---

## Custom metrics

For anything not built in, Kula accepts your own metrics over a Unix socket. See
[Custom Metrics](09-custom-metrics.md).

Next: [Custom Metrics](09-custom-metrics.md).
