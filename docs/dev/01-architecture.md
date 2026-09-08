# Architecture Overview

Kula is a single Go binary that reads kernel metrics, stores them in a custom tiered
ring-buffer, and serves them three ways: a web dashboard (SPA over HTTP + WebSocket), a
terminal UI, and a Prometheus exporter. The frontend, fonts, and locale files are embedded into
the binary via `//go:embed`, so there are no runtime assets to ship.

## Languages

- **Go** — 100% of the backend (collector, storage, web server, TUI, sandbox, config, i18n).
- **JavaScript** — the embedded SPA dashboard (ES6 modules + Chart.js).
- **HTML/CSS** — embedded static assets.
- **Bash** — build/test/release/packaging automation in `addons/`.
- **Python** — helper/operator scripts (`addons/*.py`, `scripts/*.py`).
- **YAML** — configuration.

## Runtime flow

```
                       cmd/kula/main.go
                              │
          ┌───────────────────┼───────────────────────┐
          │ serve             │ tui                    │ hash-password / inspect
          ▼                   ▼                        ▼
   ┌─────────────┐      ┌───────────┐           one-shot helpers
   │  config     │      │  collector │
   │  .Load()    │      │  + tui     │
   └──────┬──────┘      └───────────┘
          ▼
   ┌─────────────┐   ┌─────────────┐   ┌─────────────┐   ┌─────────────┐
   │  collector  │   │   storage   │   │   sandbox   │   │  web.Server │
   │  .New()     │   │  .NewStore  │   │  .Enforce   │   │  .NewServer │
   └──────┬──────┘   └──────┬──────┘   └─────────────┘   └──────┬──────┘
          │                 │                                   │
          │   every interval (default 1s):                      │
          │   sample := coll.Collect()                          │
          ├────────────────►│ store.WriteSample(sample)         │
          └────────────────────────────────────────────────────►│ server.BroadcastSample(sample)
                                                                 │
                                                          HTTP / WS / /metrics
```

`runServe` ([cmd/kula/main.go](../../cmd/kula/main.go)) wires the pieces together:

1. **Load config** (`internal/config`), apply env overrides, validate tiers.
2. **Build the collector** (`internal/collector`) and **storage store** (`internal/storage`).
3. **Build the backup scheduler** (`internal/backup`) early so a bad cron fails fast.
4. **Enforce the Landlock sandbox** (`internal/sandbox`) — restrict FS and network.
5. **Start the web server** (`internal/web`) in a goroutine (unless `web.enabled: false`).
6. Install **`signal.NotifyContext`** for `SIGINT`/`SIGTERM` and start the backup scheduler.
7. **Start application collectors** (`coll.StartApplications()`).
8. Run the **collection loop**: tick every interval → `Collect()` → `WriteSample()` →
   `BroadcastSample()`.
9. On signal: stop the collector, shut down the web server with a 5-second timeout.

## Subsystems

| Package | Responsibility | Docs |
|---------|----------------|------|
| `internal/config` | YAML parse, defaults, env overrides, tier validation | [Configuration](../user/04-configuration.md) |
| `internal/collector` | Read `/proc`, `/sys`, app endpoints into a `Sample` | [Collector](04-collector.md) |
| `internal/storage` | Tiered ring-buffer, aggregation, query | [Storage Engine](05-storage-engine.md), [Codec](06-storage-codec.md) |
| `internal/web` | HTTP server, REST API, WebSocket, auth, Ollama proxy, Prometheus | [Web Server & API](07-web-server-api.md) |
| `internal/sandbox` | Landlock filesystem + network confinement | [Sandbox](09-sandbox.md) |
| `internal/tui` | Bubble Tea terminal dashboard | [Frontend](10-frontend.md) |
| `internal/i18n` | Embedded locale lookup with English fallback | [i18n](11-i18n.md) |
| `internal/backup` | Cron-scheduled tier-file snapshots | [Backups](../user/12-backups.md) |
| `cmd/kula` | Binary entrypoint + subcommands | [CLI Reference](../user/14-cli-reference.md) |
| `cmd/kula-scan` | Out-of-tree black-box security scanner | [kula-scan](13-kula-scan.md) |
| `cmd/gen-mock-data` | Generates multi-day mock timeseries for storage tests | [Testing](12-testing.md) |

## Key design choices

- **Positional binary codec.** Samples are encoded into a compact, keyless binary record
  (float32 fields, fixed offsets) for density and speed. New metric types are appended and
  gated by preamble flag bits so old records stay decodable. See [Codec](06-storage-codec.md).
- **Tiered downsampling.** Raw 1-second data ages into 1-minute and 5-minute rollups, each in
  its own fixed-size ring buffer file. See [Storage Engine](05-storage-engine.md).
- **Stable history planning.** Source quality uses a fixed read budget independent of display
  density; output buckets use epoch-aligned nice steps and a strict point cap.
- **Kula-owned chart scheduling.** Chart.js instances sit behind one controller that batches
  updates, culls off-screen work, and derives history density from actual plot width.
- **Security in depth.** Argon2id auth, CSRF/CSP/HSTS, SRI hashes, WebSocket origin
  validation, a hardened systemd unit, *and* a Landlock sandbox enforced in-process. See
  [Security Model](08-security.md).
- **Single binary, embedded everything.** `//go:embed` pulls the SPA, fonts, locales, and
  `VERSION` into the binary.

## Dependencies

Direct module dependencies (see [go.mod](../../go.mod)):

| Module | Use |
|--------|-----|
| `github.com/gorilla/websocket` | WebSocket protocol |
| `github.com/charmbracelet/bubbletea` | TUI framework |
| `github.com/charmbracelet/lipgloss` | TUI styling |
| `github.com/charmbracelet/x/term` | Terminal raw mode (password masking) |
| `gopkg.in/yaml.v3` | YAML config parsing |
| `golang.org/x/crypto` | Argon2id |
| `golang.org/x/sys` | syscalls (adjtimex, statfs) |
| `github.com/landlock-lsm/go-landlock` | Landlock sandbox |
| `github.com/lib/pq` | PostgreSQL driver |
| `github.com/go-sql-driver/mysql` | MySQL driver |

The binary is built with `CGO_ENABLED=0` — fully static.

Next: [Project Layout](02-project-layout.md).
