# Kula

Lightweight, self-contained Linux® server monitoring tool

# Rules

1. Build script: ./addons/build.sh
2. Test suite: ./addons/check.sh (includes govulncheck, go vet, go test -v -race, golangci-lint in that order)

---

# Kula — Comprehensive Codebase Analysis

## 1. OVERALL PROJECT STRUCTURE

### Top-Level Directory
```
/home/c0m4r/ai/kula/
├── .ansible/            # Ansible deployment automation
├── .claude/             # Claude AI configuration
├── .github/             # GitHub: issue templates, contributing guide, funding, PR template
├── addons/              # Build, check, packaging, benchmark scripts, docker, init files
├── cmd/
│   ├── kula/            # Main application entrypoint (main.go, system_info.go)
│   └── gen-mock-data/   # Mock data generator tool
├── data/                # Runtime data: sessions.json, tier_*.dat (storage files)
├── dist/                # Pre-built distribution packages (.deb, .rpm, .tar.gz, AUR)
├── internal/
│   ├── collector/       # Metrics collection engine (CPU, mem, net, disk, GPU, etc.)
│   ├── config/          # YAML config parser and validator
│   ├── i18n/            # Internationalization with 26 embedded JSON locale files
│   ├── sandbox/         # Landlock Linux security sandbox
│   ├── storage/         # Tiered ring-buffer storage engine
│   ├── tui/             # Terminal UI dashboard (BubbleTea + Lipgloss)
│   └── web/             # Web server, API, WebSockets, auth, Ollama proxy, Prometheus, static UI
├── landing/             # Landing page (kula.ovh website)
├── reviews/             # Historical review documents by version
├── scripts/             # Helper scripts (nvidia-exporter, custom metrics example)
├── go.mod / go.sum      # Go module definition
├── config.example.yaml  # Configuration file example
├── VERSION / version.go # Version number (embedded via go:embed)
├── CHANGELOG.md         # Detailed changelog from v0.1.0 to present
├── SECURITY.md          # Security policy
├── LICENSE              # GNU AGPLv3
├── README.md            # Comprehensive project documentation
└── AGENTS.md            # Instructions for AI agents
```

---

## 2. LANGUAGE(S)

- **Go** — 100% of the backend. The entire binary (`cmd/kula/main.go`) is pure Go.
- **JavaScript** — Frontend SPA dashboard (embedded in binary via `//go:embed`). ES6 modules:
  - `main.js`, `auth.js`, `charts-data.js`, `gauges.js`, `ui-actions.js`, `alerts.js`, `state.js`, `ollama.js`, `game.js`, plus Chart.js library.
- **Bash** — Build/test/release automation (`addons/build.sh`, `addons/check.sh`, `scripts/nvidia-exporter.sh`, `addons/install.sh`)
- **Python** — Helper scripts (`addons/inspect_tier.py`, `addons/go_modules_updates.py`, `scripts/custom_example.py`)
- **HTML/CSS** — Embedded static assets (`index.html`, `game.html`, `style.css`)
- **YAML** — Configuration (`config.example.yaml`)

---

## 3. KEY SOURCE FILES AND THEIR PURPOSES

### Entrypoint — `cmd/kula/main.go` (~300 lines)
- Main binary: `kula serve` (daemon + web UI), `kula tui` (terminal dashboard), `kula hash-password` (Argon2 hash generator), `kula inspect` (storage inspection)
- Sets up the collector, storage engine, Landlock sandbox, web server, signal handling (SIGINT/SIGTERM via `signal.NotifyContext`)
- Collection loop runs at configurable interval (default 1s) writing samples to storage and broadcasting to WebSocket clients

### System Info — `cmd/kula/system_info.go` (~32 lines)
- Reads OS name from `/etc/os-release` and kernel version from `/proc/sys/kernel/osrelease`

### Mock Data Generator — `cmd/gen-mock-data/main.go` (~246 lines)
- Generates realistic multi-day timeseries mock data for testing storage performance boundaries

### Configuration — `internal/config/config.go` (~503 lines)
- Full YAML config parser with defaults, env var overrides (`KULA_LISTEN`, `KULA_PORT`, `KULA_DIRECTORY`, `KULA_LOGLEVEL`, `KULA_MOUNTS_DETECTION`, `KULA_POSTGRES_PASSWORD`)
- Validates storage tier hierarchy (ascending, divisible, max ratio 300:1)
- Validates Ollama URL to loopback-only (SSRF prevention)
- Config structs: `GlobalConfig`, `CollectionConfig`, `StorageConfig`, `WebConfig`, `AuthConfig`, `OllamaConfig`, `ApplicationsConfig`, `TUIConfig`

### Metrics Collection — `internal/collector/`
| File | Purpose |
|---|---|
| `collector.go` (~213 lines) | Orchestrator — calls all sub-collectors, coordinates app monitoring |
| `types.go` (~271 lines) | All data types: `Sample`, `CPUStats`, `MemoryStats`, `NetworkStats`, `DiskStats`, `GPUStats`, `ContainerStats`, `PostgresStats`, `Apache2Stats`, `PowerSupplyStats`, `NginxStats` |
| `cpu.go` (~433 lines) | CPU usage from `/proc/stat`, load averages, CPU temperature via hwmon/thermal_zone sysfs discovery |
| `disk.go` (~503 lines) | Disk I/O from `/proc/diskstats` (skips virtual/LVM/loop), filesystem usage via `statfs`, disk temperature via hwmon |
| `network.go` (~345 lines) | Network throughput from `/proc/net/dev`, TCP stats from `/proc/net/snmp` and `/proc/net/netstat` (including retrans), socket stats from `/proc/net/sockstat` |
| `system.go` (~151 lines) | Hostname, uptime, entropy, clock sync (adjtimex syscall), user count from utmp |
| `process.go` (~63 lines) | Process state counts (running, sleeping, blocked, zombie) and thread counts from `/proc/<pid>/stat` |
| `self.go` (~70 lines) | Kula's own CPU%, RSS, file descriptors |
| `gpu.go` (~182 lines) | GPU discovery via `/sys/class/drm`, supports NVIDIA, AMD, Intel |
| `gpu_nvidia.go` (~97 lines) | NVIDIA GPU metrics from `nvidia.log` (CSV format, read atomically) |
| `gpu_sysfs.go` (~81 lines) | AMD/Intel GPU metrics from sysfs (temp, power, VRAM, load) |
| `psu.go` (~102 lines) | Battery/power supply status from `/sys/class/power_supply` |
| `containers.go` (~547 lines) | Docker/Podman container monitoring via Unix socket API + cgroups v2 fallback |
| `nginx.go` (~113 lines) | Nginx stub_status monitoring (active connections, accepts/requests per second) |
| `apache2.go` (~172 lines) | Apache2 mod_status monitoring (workers, scoreboard states, per-second rates) |
| `postgres.go` (~274 lines) | PostgreSQL monitoring via `lib/pq` (connections, transactions, tuples, I/O, locking, table health, DB size) |
| `custom.go` (~194 lines) | Custom metrics via Unix domain socket (`kula.sock`) — clients send JSON |
| `ai.go` (~105 lines) | `FormatForAI()` — formats current sample as text for LLM consumption |
| `util.go` (~56 lines) | Safe parse wrappers (parseUint/parseInt/parseFloat with debug logging) |

### Storage Engine — `internal/storage/`
| File | Purpose |
|---|---|
| `store.go` (~868 lines) | Tiered storage manager — writes raw samples, triggers aggregation to higher tiers, QueryRange/QueryLatest with in-memory cache, query cache, downsampling |
| `tier.go` (~735 lines) | Ring-buffer file format: 64-byte header + variable-length records. Supports v1 (JSON) to v2 (binary) migration, wrapped segment handling, chronological ReadRange |
| `codec.go` (~1103 lines) | High-performance binary codec: 218-byte fixed block (float32-encoded CPU/mem/swap/tcp/proc/self) + variable sections (ifaces, sensors, disks, filesystems, GPU, apps). Kind-tagged `0x02` records for format detection |

### Sandbox — `internal/sandbox/sandbox.go` (216 lines)
- **Landlock LSM** enforcement using `go-landlock` library (kernel 5.13+ required)
- Restricts filesystem access to `/proc` (ro), `/sys` (ro), config file (ro), storage dir (rw)
- Restricts network to TCP bind on web port only
- Conditionally adds ConnectTCP for nginx, Apache2, MySQL, Postgres, Ollama ports
- Uses `BestEffort()` for graceful degradation on unsupported kernels

### Web Server — `internal/web/`
| File | Purpose |
|---|---|
| `server.go` (~922 lines) | HTTP server with dual-stack IPv4/IPv6 listeners, middleware chain (security, gzip, logging), API routes, template rendering, CSP nonce injection, SRI hashes |
| `auth.go` (~418 lines) | Argon2id password hashing, session management with SHA-256 token hashing, rate limiting (IP + username), CSRF protection with Origin/Referer validation and synchronizer tokens |
| `websocket.go` (~188 lines) | WebSocket handler with Origin validation, pause/resume commands, per-IP/global connection limits, ping/pong keepalive |
| `prometheus.go` (~353 lines) | `/metrics` endpoint in Prometheus text format with optional bearer token auth |
| `ollama.go` (~1049 lines) | Ollama/OpenAI-compatible AI proxy: chat streaming with SSE, model list fetch, tool-calling loop (`get_metrics` tool), rate limiting, prompt sanitization |

### Terminal UI — `internal/tui/`
| File | Purpose |
|---|---|
| `tui.go` (~221 lines) | BubbleTea model: rolling metric rings, tab navigation, collection refresh |
| `view.go` (~777 lines) | All 7 tab views (Overview, CPU, Memory, Network, Disk, Processes, GPU) with progress bars and responsive layout |
| `styles.go` (~234 lines) | Dark purple/slate theme, style caching for performance |

### Internationalization — `internal/i18n/i18n.go` (105 lines)
- Embedded 26 locale JSON files (`ar, de, en, es, fr, hi, ja, ko, pl, pt, zh, ...`)
- Translation lookup with English fallback

---

## 3a. STORAGE CODEC — METRICS ADDITION GUIDE

### Codec Architecture

The binary codec (`internal/storage/codec.go`) uses a **positional** layout — no keys, no TLV,
no length prefixes. Metrics are identified solely by their byte offset within a fixed sequence.

Each record has this structure:

```
┌──────────────────────────────────────────────────────────┐
│  Preamble (18 bytes)                                     │
│  [0:8]   Timestamp (int64, nanoseconds)                  │
│  [8:16]  Duration  (int64, nanoseconds)                  │
│  [16:18] Flags     (uint16 bitmask)                      │
│          flagHasMin     = 1 << 0                         │
│          flagHasMax     = 1 << 1                         │
│          flagHasData    = 1 << 2                         │
│          flagHasApps    = 1 << 3   (gate: app section)   │
│          flagHasApache2 = 1 << 8   (gate: Apache2 block) │
│          flagHasMysql   = 1 << 9   (gate: MySQL block)   │
│          ... new flags: 1 << 10, 1 << 11, ...            │
├──────────────────────────────────────────────────────────┤
│  Fixed block (218 bytes) — CPU, memory, swap, TCP,       │
│  process, self metrics. Always the same size.            │
├──────────────────────────────────────────────────────────┤
│  Variable section — sequential:                          │
│    1. Network interfaces (count + per-iface data)        │
│    2. CPU temp sensors (count + per-sensor data)         │
│    3. Disk devices (count + per-device data)             │
│    4. Filesystems (count + per-fs data)                  │
│    5. System strings (hostname, clocksource)             │
│    6. GPU entries (count + per-GPU data)                 │
│                                                          │
│    7. Application metrics — ordered, fixed sequence:     │
│       a. Nginx       (1 byte presence + 52 bytes data)   │
│       b. Containers  (2 bytes count + variable per-ct)   │
│       c. PostgreSQL  (1 byte version + 56/104 bytes)     │
│       d. MySQL       (1 byte version + 56 bytes)         │
│       e. Apache2     (1 byte version + 72/100 bytes)     │
│       f. Custom      (2 bytes group count + variable)    │
│                                                          │
│       ← NEW METRIC TYPES MUST BE INSERTED HERE,          │
│         BEFORE "Custom"                                   │
└──────────────────────────────────────────────────────────┘
```

### The Rule

**New fixed-size application metric types MUST be appended after all existing fixed
sections and before the trailing Custom section.** Never insert a new section between
existing ones.

Each new type is gated by a dedicated preamble flag bit. The decoder checks each flag:
if absent (old record), that section's bytes are skipped entirely and subsequent
sections remain correctly aligned.

Incrementing a single metric type's block size (adding fields) uses a version-tagged
presence byte. e.g. `0` = absent, `1` = v1 (old size), `2` = v2 (new size). The
decoder reads the version and dispatches to the correct block layout. This does NOT
break old records because the section's position in the sequence doesn't change.

---

### How a New Flag Protects Old Records

Consider a record written before Apache2 existed:

```
Preamble flags: hasApps=1, hasApache2=0   ← Apache2 flag absent

decodeVariable with (hasApps=true, hasApache2=false):
  nginx:       read 1 byte  → done
  containers:  read 2 bytes → done
  postgres:    read 1 byte  → done
  mysql:       read 1 byte  → done
  if hasApache2 → FALSE, skip this entire section  ← CORRECT
  custom:      read 2 bytes + groups  → done
```

The decoder knows exactly which bytes belong to which section because the order is
deterministic. A missing flag means "pretend this section doesn't exist and move on."

---

### Step-by-Step Checklist

Use this checklist when adding a new application metric type (e.g. Redis).

The example below assumes a new type called `Foo` with a 48-byte block.

#### 1. Config (`internal/config/config.go`)

Add a config struct and wire it into `ApplicationsConfig`:

```go
type FooConfig struct {
    Enabled   bool   `yaml:"enabled"`
    StatusURL string `yaml:"status_url"`
}

type ApplicationsConfig struct {
    // ...existing fields...
    Foo FooConfig `yaml:"foo"`
}
```

Set defaults in `DefaultConfig()`.

#### 2. Types (`internal/collector/types.go`)

Add the stats struct with JSON tags:

```go
type FooStats struct {
    MetricA int     `json:"metric_a"`
    MetricB float64 `json:"metric_b"`
    // ...
}
```

Add `Foo *FooStats` to `ApplicationsStats`.

#### 3. Collector (`internal/collector/foo.go`)

Implement `collectFoo(elapsed float64) *FooStats`. Follow the nginx collector pattern:
lazy-allocated HTTP client, error handling returning nil, parse the upstream format.

If the metric source has cumulative counters that can reset on restart, guard the delta
computation against counter rollback (see `nginx.go:91` and `apache2.go:129`).

#### 4. Wire into orchestrator (`internal/collector/collector.go`)

- Add `fooClient *http.Client` and `prevFoo fooRaw` to the `Collector` struct.
- Add init log: `if appCfg.Foo.Enabled { log.Printf("[foo] monitoring enabled at %s", ...) }`
- Add dispatch in `collectApps()`: `if c.appCfg.Foo.Enabled { apps.Foo = c.collectFoo(elapsed) }`

#### 5. Sandbox (`internal/sandbox/sandbox.go`)

If the collector makes outbound HTTP connections, add a `ConnectTCP` rule for the port:

```go
if appCfg.Foo.Enabled && appCfg.Foo.StatusURL != "" {
    if u, err := url.Parse(appCfg.Foo.StatusURL); err == nil {
        port := 80
        if u.Port() != "" {
            if p, err := strconv.Atoi(u.Port()); err == nil && p > 0 && p <= 65535 {
                port = p
            }
        } else if u.Scheme == "https" {
            port = 443
        }
        netRules = append(netRules, landlock.ConnectTCP(uint16(port)))
        appInfo = append(appInfo, fmt.Sprintf("foo:connect-tcp/%d", port))
    }
}
```

#### 6. Preamble flag (`internal/storage/codec.go`)

Add a new flag constant **before** the fixed block size constant:

```go
const (
    flagHasMin     uint16 = 1 << 0
    flagHasMax     uint16 = 1 << 1
    flagHasData    uint16 = 1 << 2
    flagHasApps    uint16 = 1 << 3
    flagHasApache2 uint16 = 1 << 8
    flagHasMysql   uint16 = 1 << 9
    flagHasFoo     uint16 = 1 << 10  // <-- NEW
)
```

Always set it in `appendPreamble()` — new records always carry the flag:

```go
flags |= flagHasFoo
```

#### 7. Encode block (`internal/storage/codec.go` — `appendVariable`)

**Append** the new section after MySQL and before Custom. Place it at the end of the
fixed app sections:

```
nginx → containers → postgres → mysql → apache2 → foo → custom
                                                      ^^^^^
```

```go
// Foo (1-byte presence + 48-byte fixed block when present)
if s.Apps.Foo != nil {
    buf = append(buf, 1)
    f := s.Apps.Foo
    var fb [48]byte
    // ...binary.LittleEndian.PutUint32(...)...
    buf = append(buf, fb[:]...)
} else {
    buf = append(buf, 0)
}
```

When the block size grows in the future, bump the presence tag to `2` and add
version-tagged decoding.

#### 8. Decode block (`internal/storage/codec.go` — `decodeVariable`)

Gate the new section behind the flag extracted from the preamble. **Append** after
Apache2 and before Custom:

```go
// Foo — gated by flagHasFoo so old records skip this byte.
if hasFoo {
    fooPresent := data[off]; off++
    if fooPresent != 0 {
        if err := need(48, "foo fields"); err != nil {
            return off, err
        }
        f := &collector.FooStats{}
        f.MetricA = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
        // ...
        s.Apps.Foo = f
    }
}
```

Extract the flag in `decodeSample()` and thread it through `decodeVariable()`:

```go
hasFoo := flags&flagHasFoo != 0
// ...
vn, err := decodeVariable(data[off:], s, hasApps, hasApache2, hasMysql, hasFoo)
```

Update the `decodeVariable` signature to accept the new `hasFoo bool` parameter.
Update all call sites (tests included).

#### 9. Store aggregation (`internal/storage/store.go`)

- **Deep copy** on init: add `if last.Apps.Foo != nil { ... }` alongside nginx/apache2.
- **Rate averaging**: average per-second rate fields across aggregated samples (same
  pattern as nginx at `store.go:680`).

#### 10. Python decoder (`addons/inspect_tier.py`)

- Add the flag constant: `FLAG_HAS_FOO = 1 << 10`
- Extract `has_foo` from flags and pass to `_decode_variable()`.
- Add the Foo decoding block at the same position (after Apache2, before Custom).
- Gate with `if has_foo:`.

#### 11. Frontend charts (`internal/web/static/js/app/charts-data.js`)

- Add an `APP_ORDER_FOO` constant with a unique value (increment by 10).
- Create charts dynamically on first data: `if (s.apps?.foo) { ... }`.
- Register chart card IDs in `charts-init.js` `destroyAppCharts()` for cleanup.

#### 12. Config documentation (`config.example.yaml`)

Add the config section with comments explaining prerequisites.

#### 13. Tests (`internal/collector/app_test.go`)

- Test valid parse output.
- Test malformed output returns nil.
- Test counter reset doesn't produce insane rates (if cumulative counters apply).

#### 14. Verify

```bash
./addons/check.sh
```

All four checks must pass: govulncheck, go vet, go test -race, golangci-lint.

---

### Quick Reference: Available Flag Bits

| Bit | Flag | Purpose |
|-----|------|---------|
| 0   | `flagHasMin`     | Min block present |
| 1   | `flagHasMax`     | Max block present |
| 2   | `flagHasData`    | Data block present |
| 3   | `flagHasApps`    | Application metrics section present |
| 8   | `flagHasApache2` | Apache2 block present |
| 9   | `flagHasMysql`   | MySQL block present |
| 10  | —                | Next available |
| 11  | —                | Available |
| ... | —                | Available up to bit 15 |

Use bit 10 for the next metric type. Bits 4–7 and 10–15 are free. Do not reuse bits.

---

## 4. CONFIGURATION FILES AND DEPENDENCIES

### `go.mod` — Direct Dependencies:
| Package | Purpose |
|---|---|
| `github.com/gorilla/websocket` | WebSocket protocol |
| `github.com/charmbracelet/bubbletea` | TUI framework |
| `github.com/charmbracelet/lipgloss` | TUI styling |
| `github.com/charmbracelet/x/term` | Terminal raw mode |
| `gopkg.in/yaml.v3` | YAML config parsing |
| `golang.org/x/crypto` | Argon2id password hashing |
| `golang.org/x/sys` | System calls (adjtimex, statfs) |
| `github.com/landlock-lsm/go-landlock` | Linux Landlock sandbox |
| `github.com/lib/pq` | PostgreSQL driver |
| `github.com/go-sql-driver/mysql` | MySQL driver |

### Config Files:
- **`config.example.yaml`** — Template with all defaults (~238 lines)
- **`VERSION`** — current version number

### Build/Test Scripts:
- **`addons/build.sh`** — Single or cross-compile (amd64, arm64, riscv64) with `-trimpath -ldflags="-s -w"`
- **`addons/check.sh`** — Runs govulncheck, go vet, go test -v -race, golangci-lint
- **`addons/install.sh`** — Guided installation script (~373 lines, multi-distro support)
- **`addons/benchmark.sh`** — Storage engine benchmark suite with pretty-printed output

---

## 5. SECURITY-RELATED CODE (Comprehensive)

### Authentication & Password Storage
- **Argon2id** password hashing (`internal/web/auth.go`) with configurable parameters (memory: 32MB, time: 3, threads: 4 — double OWASP minimum)
- Configurable **multiple user support** (`config.AuthConfig.Users`)
- **Constant-time comparison** (`crypto/subtle.ConstantTimeCompare`) for both username and password hash verification
- Password reading in `hash-password` mode uses **raw terminal mode** with asterisk masking (not plaintext echo)

### Session Management
- **Token-only validation** — sessions are NOT bound to client IP or User-Agent (tested in `auth_test.go`)
- **SHA-256 session token hashing** — plaintext tokens on wire, only hashes stored on disk (`sessions.json`)
- **Sliding expiration** — successful validation extends the session
- **Secure cookies**: `HttpOnly`, `SameSite=StrictMode`, `Secure` flag conditional on TLS/X-Forwarded-Proto
- **Bearer token** support in `Authorization` header
- **Rate limiting**: 5 login attempts per 5 minutes per IP AND per username
- **Session cleanup** goroutine runs every 5 minutes to purge expired sessions

### CSRF Protection
- **Origin/Referer validation** for ALL non-GET/HEAD/OPTIONS requests (`ValidateOrigin` in auth.go)
- **Synchronizer token** pattern — CSRF tokens sent in `X-CSRF-Token` header, validated via constant-time compare
- Empty Origin headers now **rejected** (fixed in 0.9.1)

### Landlock Sandbox (v0.4.0+)
- Filesystem: `/proc` and `/sys` read-only, config file read-only, storage dir read-write, `/etc/hosts`/`/etc/resolv.conf`/`/etc/nsswitch.conf` read-only
- Network: Only TCP bind on configured web port, plus conditional ConnectTCP for nginx/Apache2/MySQL/Postgres/Ollama ports
- Checks Landlock ABI version at startup, gracefully degrades on older kernels

### Web Security Headers
- `X-Content-Type-Options: nosniff`
- `X-Frame-Options: DENY`
- `Content-Security-Policy` with random nonce per request (`default-src 'self'; script-src 'self' 'nonce-<random>'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'`)
- `Referrer-Policy: strict-origin-when-cross-origin`
- `Permissions-Policy: geolocation=(), microphone=(), camera=()`
- **HSTS** (`Strict-Transport-Security`) when TLS or trusted `X-Forwarded-Proto: https` is present (added in 0.15.0)

### SRI (Subresource Integrity)
- All JavaScript files are served with `integrity="sha384-..."` hashes computed at startup via `sha512.Sum384` (in `calculateSRIs`)
- Hashes injected into templated HTML pages

### WebSocket Security
- **Origin validation** for WebSocket upgrades (prevents CSWSH) — non-browser clients allowed without Origin header
- **Global connection limit** (default 100) and **per-IP limit** (default 5)
- **Read limit** of 4096 bytes on incoming WebSocket JSON messages
- 60-second read deadline with pong handler refresh
- **sync.Once** for unregister to prevent double-counting bugs

### Input Validation & Sanitization
- **`json.Marshal` for error responses** (not `fmt.Sprintf` — prevents JSON injection) (`jsonError` in server.go)
- **Request body size limits**: login body max 4096 bytes, Ollama chat max 32KB
- **Time range caps**: max 31 days in `/api/history`, max 5000 data points
- **Directory traversal prevention**: storage path resolved to absolute path via `filepath.Abs`
- **Password masking** with asterisks in hash-password mode (uses terminal raw mode)

### Ollama AI Security
- **SSRF prevention**: Ollama URL validated to only allow loopback addresses (`localhost`, `127.0.0.1`, `::1`) at config load time
- **Prompt sanitization**: null bytes stripped, length clamped to 2000 runes, whitespace trimmed
- **Model name validation**: regex `^[A-Za-z0-9._:/-]{1,200}$` — rejects shell metacharacters, spaces, backticks
- **Rate limiting**: 10 chat requests/IP/minute, 60 meta requests/IP/minute
- **Response size limit**: 10MB max Ollama stream
- **Tool loop limit**: max 5 tool-call rounds per chat turn

### Prometheus Metrics Security
- Optional **bearer token** authentication for `/metrics` endpoint
- Constant-time comparison for token validation

### Config Security
- **PostgreSQL password**: single-quoted and escaped (backslashes and single quotes escaped) to prevent libpq injection via `KULA_POSTGRES_PASSWORD` env var (added in 0.15.1)
- **Storage directory permissions**: created with `0750`
- **Session file permissions**: saved with `0600`

### Other Security Measures
- **HTTP server timeouts**: ReadTimeout 30s, WriteTimeout 60s, IdleTimeout 120s
- **TLS conditional HSTS** based on connection or trusted proxy header
- **`X-Forwarded-For` trust**: uses rightmost IP in the chain (most trusted)
- **Governance**: `SECURITY.md` with private vulnerability reporting, `CODE_OF_CONDUCT.md`, `CONTRIBUTING.md`

### Testing Coverage
All security-critical code has dedicated tests:
- `auth_test.go` — password hashing determinism, salt generation, credential validation (enabled/disabled), session lifecycle (create/validate/expire/cleanup), session hashing on disk, legacy session loading, client IP extraction, Origin validation, CSRF middleware
- `server_test.go` — template injection prevention (nonce/CSP), SRI verification
- `websocket_test.go` — connection limits (global + per-IP)
- `ollama_test.go` — model name validation, prompt sanitization, rate limiting, tool execution
- `prometheus_test.go` — bearer token auth, empty store, label escaping
- `sandbox_test.go` — write outside storage, execute outside paths, external network dial (all expected to fail)
- `config_test.go` — YAML parsing, env overrides, tier validation