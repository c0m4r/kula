# Configuration

All settings live in `config.yaml`. Kula looks for it via the `-config` flag (default
`config.yaml` in the working directory). A fully-commented template is shipped as
[`config.example.yaml`](../../config.example.yaml); copy it and edit.

Every section is optional — omitted settings fall back to built-in defaults.

If you start Kula with an explicit `-config <path>` that doesn't exist yet, Kula seeds that
path from the packaged example config (the same `config.example.yaml`, embedded in the binary
at build time) and logs a warning, rather than refusing to start. The file is written with
`0600` permissions; edit it and restart to apply your settings.

## Environment variable overrides

A handful of settings can be overridden by environment variables, which take precedence over
the file. These are handy for containers and quick tweaks:

| Variable | Overrides | Notes |
|----------|-----------|-------|
| `KULA_DIRECTORY` | `storage.directory` | Where tier files are written |
| `KULA_LOGLEVEL` | `web.logging.level` | `access`, `perf`, or `debug` |
| `KULA_LISTEN` | `web.listen` | Bind address |
| `KULA_PORT` | `web.port` | Bind port |
| `KULA_UNIX_SOCKET` | `web.unix_socket` | Listen on a Unix socket instead of TCP |
| `KULA_MOUNTS_DETECTION` | `collection.mounts_detection` | `auto`, `host`, or `self` |
| `KULA_BASE_PATH` | `web.base_path` | URL sub-path prefix |
| `KULA_POSTGRES_PASSWORD` | `applications.postgres.password` | Injected safely (escaped) |

---

## `global`

```yaml
global:
  hostname: ""            # Override reported hostname (default: system hostname)
  show_system_info: true  # Show OS / Kernel / Architecture in UI
  show_version: true      # Show Kula version in UI
  default_theme: auto     # Web UI theme: light, dark, or auto
  easter_egg: true        # Show the Space Invaders button in the UI
```

When `show_system_info` is `false`, OS/Kernel/Arch are reported as "Hidden".

---

## `collection`

```yaml
collection:
  interval: 1s            # 1s, 2s, 5s, 10s, 15s, or 30s
  mounts_detection: auto  # auto | host | self
  # devices: ["sda", "nvme0n1"]      # override auto-detected disks
  # mountpoints: ["/", "/mnt/data"]  # override auto-detected filesystems
  # interfaces: ["eth0", "wlan0"]    # override auto-detected NICs
```

- **`interval`** must be one of the allowed values and **must match Tier 1's resolution**.
- **`mounts_detection`** controls how mount points are discovered:
  - `auto` — merges host and container mounts (detects namespaces).
  - `host` — reads only `/proc/1/mounts` (host-level visibility).
  - `self` — reads only `/proc/self/mounts` (container-level visibility).
- The `devices`/`mountpoints`/`interfaces` lists let you pin exactly what is monitored,
  bypassing auto-discovery.

---

## `storage`

```yaml
storage:
  directory: /var/lib/kula   # falls back to ~/.kula on permission failure
  tiers:
    - resolution: 1s         # Tier 1 (raw) — must equal collection.interval
      max_size: 250MB
    - resolution: 1m         # Tier 2 — 1-minute aggregation
      max_size: 150MB
    - resolution: 5m         # Tier 3 — 5-minute aggregation
      max_size: 50MB
```

Tier rules enforced at startup:

- Resolutions must be **strictly ascending** (Tier 1 < Tier 2 < Tier 3).
- Each higher tier's resolution must be **divisible** by the lower one.
- The ratio between adjacent tiers is capped (max 300:1) to bound memory used by aggregation
  buffers.

Changing the default resolutions can cause unexpected behavior, and very coarse Tier 2/3
resolutions raise memory use and risk losing buffered samples on shutdown. See
[Storage Engine](../dev/05-storage-engine.md) for internals.

---

## `backup`

Scheduled snapshots of the tier files. See [Backups](12-backups.md).

```yaml
backup:
  enabled: false       # master switch
  cron: "0 0 * * *"    # 5-field crontab expression (default: midnight)
  maxtier: 3           # how many tiers to back up (1 = raw only, 3 = all)
  retention: 1d        # keep backups this long; supports s/m/h/d; empty = no pruning
  compress: true       # gzip each backed-up tier file
```

Backups are written under `<storage.directory>/backup/<timestamp>/`.

---

## `web`

The largest section. Controls the HTTP server, dashboard, and security.

```yaml
web:
  enabled: true           # master switch for the web server
  ui: true                # serve dashboard + API (false = only /metrics + /health)
  listen: ""              # use [] for IPv6, e.g. "[::1]"
  port: 27960
  base_path: ""           # mount under a URL prefix, e.g. "/kula"
  # unix_socket: /run/kula/kula.sock   # listen on a Unix socket (no TCP listener)
  # unix_socket_mode: "0660"
  enable_compression: true
  max_websocket_conns: 100
  max_websocket_conns_per_ip: 5
  trust_proxy: false      # trust X-Forwarded-Proto from a reverse proxy
```

Key points:

- **`enabled: false`** does not open any network listener at all.
- **`ui: false`** keeps Prometheus metrics and health endpoints but disables the dashboard
  and its JSON API.
- **`base_path`** serves *every* route (UI, API, WebSocket, `/metrics`, `/health`) under a
  prefix — for reverse proxies that forward the prefix intact. See
  [Reverse Proxy & TLS](13-reverse-proxy.md).
- **`unix_socket`** replaces the TCP listener — ideal behind a local nginx.

### `web.security`

```yaml
  security:
    headers: true            # emit CSP, X-Content-Type-Options, HSTS, etc.
    frame_protection: true   # X-Frame-Options: DENY + CSP frame-ancestors 'none'
    origin_validation: true  # reject cross-origin state-changing requests / WS upgrades
    # allowed_origins:
    #   - https://app.example.com
```

Defaults preserve strict behavior. Relax these to embed Kula in an `<iframe>` or to allow
cross-origin browser access. When `allowed_origins` is non-empty, CORS headers are sent for
matching origins, those origins pass `origin_validation`, and session cookies switch to
`SameSite=None; Secure` (requires HTTPS or `trust_proxy` + `X-Forwarded-Proto: https`). See
[Security Model](../dev/08-security.md).

### `web` display options

```yaml
  join_metrics: false        # connect across gaps in graphs (false = show gaps)
  default_aggregation: max   # historical aggregation: avg | min | max
  lang:
    default: en              # ar de en es fr hi ja ko pl pt zh
    force: false             # hide the language selector
  graphs:
    cpu_temp:  { max_mode: "off", max_value: 100 }   # Celsius
    disk_temp: { max_mode: "off", max_value: 100 }
    network:   { max_mode: "off", max_value: 1000 }  # Mbps
    split:
      network: false      # one chart per interface
      disk_io: false      # one chart per disk
      disk_space: false   # one chart per mount point
      disk_temp: false    # one chart per disk (thermals)
      gpu: false          # one chart per GPU
```

`max_mode` per graph:

- `"off"` — Chart.js auto-scales the Y-axis to the data peaks.
- `"on"` — imposes a hard Y-axis maximum equal to `max_value`.
- `"auto"` — tries to detect a hardware limit (CPU TjMax, network link speed); falls back to
  `max_value` if detection fails.

`split` toggles can also be flipped per-chart from the dashboard using the split (⊟) button.

### `web.logging`

```yaml
  logging:
    enabled: true
    level: "perf"   # access | perf | debug
```

- `access` — request lines (`[API]` / `[WEB]` tags, client IP, method, path, status).
- `perf` — adds storage fetch performance for `/api/history`.
- `debug` — adds collector auto-discovery logging.

### `web.prometheus_metrics`

```yaml
  prometheus_metrics:
    enabled: false
    token: ""   # optional bearer token; scrapers send Authorization: Bearer <token>
```

See [Prometheus Exporter](11-prometheus.md).

### `web.auth`

See [Authentication](07-authentication.md).

```yaml
  auth:
    enabled: false
    username: admin
    password_hash: ""   # from: ./kula hash-password
    password_salt: ""
    session_timeout: 24h
    session_max_lifetime: 168h  # absolute cap from login; 0 disables
    argon2:
      time: 3
      memory: 32768     # KiB (double the OWASP minimum)
      threads: 4
    # users:            # additional users, same argon2 params
    #   - username: user1
    #     password_hash: ""
    #     password_salt: ""
```

---

## `applications`

Per-application monitoring modules, each independently toggled. Full setup details are in
[Application Monitoring](08-application-monitoring.md) and [Custom Metrics](09-custom-metrics.md).

```yaml
applications:
  nginx:
    enabled: false
    status_url: "http://localhost/status"
  apache2:
    enabled: false
    status_url: "http://localhost/server-status?auto"
  containers:
    enabled: true
    # socket_path: "/var/run/docker.sock"
    # containers: ["my-app", "postgres-db"]  # filter by name/ID prefix
  custom:
    # cpu_fans:
    #   - { name: fan1, unit: RPM, max: 5000 }
  postgres:
    enabled: false
    host: "localhost"
    port: 5432
    user: "kula_monitor"
    password: ""
    dbname: "postgres"
    sslmode: "disable"
  mysql:
    enabled: false
    host: "127.0.0.1"
    port: 3306
    user: "kula_monitor"
    password: ""
    dbname: ""
```

---

## `tui`

```yaml
tui:
  refresh_rate: 1s
```

Refresh interval for the terminal UI (independent of the collection interval).

---

## `ollama`

Local AI assistant. See [AI Assistant](10-ai-assistant.md).

```yaml
ollama:
  enabled: false
  url: "http://localhost:11434"   # MUST be loopback (SSRF protection)
  model: "gemma4:e4b"
  timeout: "300s"
```

The Ollama URL is validated to loopback addresses only (`localhost`, `127.0.0.1`, `::1`) at
config load time to prevent server-side request forgery.

---

Next: [Web Dashboard](05-web-dashboard.md).
