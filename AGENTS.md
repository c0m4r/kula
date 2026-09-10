# Kula

Lightweight, self-contained Linux® server monitoring tool. A single CGO-free Go binary samples
`/proc` and `/sys` every second, stores samples in a tiered ring-buffer storage engine, and
serves a web dashboard, a terminal UI, and a Prometheus endpoint. No external database and no
runtime dependency.

## Commands

| Task | Command |
|---|---|
| Build (current arch) | `./addons/build.sh` |
| Cross-compile amd64 / arm64 / riscv64 | `./addons/build.sh cross` |
| Full gate — required before a change is done | `./addons/check.sh` |
| Browser frontend regressions (needs node + Chromium) | `./addons/test-frontend-regressions.sh` |
| Native fuzzing over every `Fuzz*` target | `./addons/fuzz.sh [duration] [filter]` |
| Storage engine benchmarks | `./addons/benchmark.sh` |

`./addons/check.sh` runs, in order: `govulncheck` → `gofmt -l .` → `go vet ./...` →
`go test -v -race ./...` → `golangci-lint`. govulncheck and golangci-lint print "Skipping" when
not installed; gofmt, vet and the race tests always run and must be clean. Run `gofmt -w .`
before the gate. Tests run with `-race` — a plain `go test ./...` pass is not the gate.

## Invariants

Each item below is enforced by a test, or fails silently and corrupts stored history.

### Storage codec — positional, append-only, flag-gated

`internal/storage/codec.go`. Records have no keys, no TLV, no length prefixes: every metric is
identified by its byte offset in a fixed sequence, so **section order is the contract**.

- A new metric type gets a **new preamble flag bit** and its section is appended **after every
  existing section** (after disk IDs). Never insert a section between existing ones.
- Next free flag bit: **13**. Used: 0–4, 8–12. Free: 5–7, 13–15. Never reuse a bit.
- Growing an existing block does not add a flag: bump that section's **version-tagged presence
  byte** (`0` absent, `1` v1, `2` v2) and dispatch on it. The section's position never moves.
- The record-level contributing-statistics trailer (bit 11) follows **all** Data/Min/Max blocks.
- Mirror every codec change in the Python decoder `addons/inspect_tier.py` (same flag constants,
  same order) or it mis-decodes records.
- `FuzzDecodeSample` must never panic on malformed input.

Full walkthrough: [Adding a Metric Type](docs/dev/14-adding-metrics.md),
[Binary Codec](docs/dev/06-storage-codec.md).

### Aggregation — schema-driven

Declare a policy for **every numeric field** of a collector type with an `agg` struct tag:
`mean` (duration-weighted), `mean_nonnegative` (excludes negative unavailable sentinels), or
`last` (monotonic counters, identities, fixed capacities). Mark the stable key of each element in
a dynamic slice with `identity` (`identity_fallback` only when the primary key can be empty).
`TestAggregationPoliciesCoverSampleSchema` fails on any omission. Do not hand-write deep-copy or
per-application reducer branches.

### Collectors

- **Return `nil` on failure** — never break the whole sample because one subsystem is missing.
- For cumulative counters that reset on restart, guard the delta against rollback
  (`internal/collector/nginx.go`, `internal/collector/apache2.go`). A reset must not produce an
  absurd rate.
- A new outbound connection needs a Landlock `ConnectTCP` rule in `internal/sandbox/sandbox.go`,
  or the sandbox blocks it at runtime.

### Embedded assets and i18n

- `internal/web/static/`, `internal/i18n/locales/`, `VERSION` and `config.example.yaml` are
  `//go:embed`ed — rebuild for edits to take effect.
- SRI hashes are computed at startup by walking every `static/**/*.js`, so a new script is
  covered automatically; it still needs its template tag and CSP clearance.
- 26 locale files. `TestCurrentUITranslationsCoverEveryLocale` requires every locale to carry all
  `si_*` keys present in `internal/i18n/locales/en.json` plus a fixed required list, and rejects
  obsolete `si_*` keys. Adding, renaming or removing a UI string means editing **all** locale
  files.

### Build and lint

- **CGO-free** (`CGO_ENABLED=0`). Do not introduce cgo dependencies.
- `goimports` uses `local-prefixes: kula`, so project imports form their own group. `gofumpt` is
  deliberately disabled — do not enable it (the module path `kula` has no dot, so gofumpt
  classifies project imports as stdlib and fights goimports in an unfixable loop).
- Security defaults stay strict (headers, CSRF, rate limits, sandbox). Relaxations are opt-in
  through config, never the new default.
- Keep dependencies minimal — self-containment is the product.

## Where things live

| You want to change… | Edit |
|---|---|
| A metric or its source parsing | `internal/collector/` (plus the codec, via the guide above) |
| Record format, tiers, aggregation | `internal/storage/` |
| HTTP routes, API, middleware | `internal/web/server.go` |
| Auth, sessions, CSRF | `internal/web/auth.go` |
| AI proxy / Prometheus endpoint | `internal/web/ollama.go`, `internal/web/prometheus.go` |
| Dashboard SPA | `internal/web/static/js/app/` |
| Terminal UI | `internal/tui/` |
| Config schema and defaults | `internal/config/config.go` + `config.example.yaml` |
| Filesystem / network confinement | `internal/sandbox/sandbox.go` |
| Translations | `internal/i18n/locales/` |
| Tier-file inspector (Python) | `addons/inspect_tier.py` |
| Black-box security scanner binary | `cmd/kula-scan/` |

Two binaries beyond `cmd/kula/`: `cmd/kula-scan/` (scanner, imports nothing from `internal/`)
and `cmd/gen-mock-data/` (multi-day fixture generator).

## Documentation

The `docs/` tree is the deep reference: read the relevant page before re-deriving behavior from
code, and trust the code when the two disagree.

| Topic | Page |
|---|---|
| Architecture, project layout | `docs/dev/01-architecture.md`, `docs/dev/02-project-layout.md` |
| Build, test, fuzz, benchmarks | `docs/dev/03-building.md`, `docs/dev/12-testing.md` |
| Collector subsystem | `docs/dev/04-collector.md` |
| Storage engine, binary codec | `docs/dev/05-storage-engine.md`, `docs/dev/06-storage-codec.md` |
| Web server & API, security, sandbox | `docs/dev/07-web-server-api.md`, `docs/dev/08-security.md`, `docs/dev/09-sandbox.md` |
| Frontend, i18n | `docs/dev/10-frontend.md`, `docs/dev/11-i18n.md` |
| kula-scan | `docs/dev/13-kula-scan.md` |
| Adding a metric type | `docs/dev/14-adding-metrics.md` |
| Packaging, release, contributing | `docs/dev/15-packaging.md`, `docs/dev/16-contributing.md` |
| User-facing behavior and config reference | `docs/user/` |

When behavior changes, update the page that documents it, and `config.example.yaml` plus
`README.md` when the change is user-facing.
