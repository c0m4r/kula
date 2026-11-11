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
| GitHub Actions workflows locally, in Docker | `./addons/ci-local.sh [run WORKFLOW [JOB]]` |
| Native fuzzing over every `Fuzz*` target | `./addons/fuzz.sh [duration] [filter]` |
| Storage engine benchmarks | `./addons/benchmark.sh` |
| Rebuild the vendored Chart.js bundle (needs node + npm) | `./addons/build-chartjs.sh [--update]` |
| Packaging helpers still leave a tree that vets and passes the web tests | `./addons/packaging/check_helpers.sh` |

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
  existing section** (after the TCP extension). Never insert a section between existing ones.
- Next free flag bit: **14**. Used: 0–4, 8–13. Free: 5–7, 14–15. Never reuse a bit.
- Growing an existing block does not add a flag: bump that section's **version-tagged presence
  byte** (`0` absent, `1` v1, `2` v2) and dispatch on it. The section's position never moves.
- The record-level contributing-statistics trailer (bit 11) follows **all** Data/Min/Max blocks.
- Mirror every codec change in the Python decoder `addons/inspect_tier.py` (same flag constants,
  same order) or it mis-decodes records.
- Records are written as kind `0x03`: the encoded sample framed by `encodeSampleV` with a
  CRC-32 trailer over the length prefix and payload (`record_checksum.go`). Kind `0x02`
  (pre-0.22.0, no checksum) stays readable. Decode tier records only through
  `Tier.readRecord`, which verifies the checksum; calling `decodeSample` on a payload skips it.
- `FuzzDecodeSample` and `FuzzReadRecord` must never panic on malformed input.
- `TestCodecPersistsEveryAggregatedNumericField` fails when a numeric field with an `agg`
  policy does not survive encode → decode; a new field needs a codec home, not just a tag.

Full walkthrough: [Adding a Metric Type](docs/dev/14-adding-metrics.md),
[Binary Codec](docs/dev/06-storage-codec.md).

### Tier seek index

`internal/storage/tier_index.go` keeps a sparse record index per tier and saves it at clean
shutdown as `tier_N.idx`, adopted only when it matches the tier header exactly. `Tier.Write`
maintains it; any other code that writes records into an open tier (test helpers simulating
older writers) must go through `writePayload`, which calls `seekAppend` like `Write` does, or
seeking scans skip those records.
`TestSeekIndexScansMatchFullScans` requires seeking scans to equal full scans; read-only
tiers (the TUI) build and extend their own index, checked by
`TestReadOnlySeekIndexMatchesFullScans` and `TestReadOnlySeekIndexSurvivesAnUnseenLap`.
The index also tracks each pass's newest backstep (a record written below an earlier
timestamp, after the clock stepped back) and greatest timestamp: a scan stops at the first
record past its window only beyond the backstep, and skips the previous pass only when its
greatest timestamp precedes the window (`TestScansFindRecordsWrittenAfterAClockStep`,
`TestScansReadThePreviousPassAcrossAClockStep`). Without an index a scan reads every record.

`Tier.SnapshotTo` (backups) copies a tier without its lock, then re-copies only the header and
the ring range written since. Code that changes any other byte of an open tier file at runtime
must extend that patch, or backups silently diverge (`TestSnapshotCopiesWritesMadeDuringTheCopy`).

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
- Served `.css`/`.js` (except vendored `*.min.*`) is minified at startup by
  `internal/web/minify.go`, and SRI hashes cover the minified bytes. `TestEmbeddedAssetsMinify`
  fails if a shipped asset no longer minifies (it would silently be served unminified). Tests
  that assert on source text must read `staticFS`, not the served response.
- 26 locale files. `TestCurrentUITranslationsCoverEveryLocale` requires every locale to carry
  every key of `internal/i18n/locales/en.json`, non-empty, and rejects keys `en.json` no longer
  has. Adding, renaming or removing a UI string means editing **all** locale files.

### Dashboard chart lifecycle

- Off-screen charts defer layout and drawing, but must drain Chart.js mutation records during
  controller flushes. `chart-controller.js` calls the vendored private `_updateHiddenIndices()`
  method to preserve hidden-point indices when data is trimmed; do not simply clear
  `_dataChanges`. The x-axis tick callback in `charts-init.js` reads the private time-scale
  `_unit`. `TestChartBundle` asserts both exist; recheck them when upgrading Chart.js.
- Dashboard cards enter the page only through `card-layout.js` `placeCard()`, and only
  `arrangeCards()` moves them. The layout must stay a function of the cards present, the stored
  order (`kula_card_order`) and the mode. Never append a card yourself or give it a persistent
  CSS `order`; expand's temporary orders are the exception. A new application group goes in
  `APP_GROUPS` (`card-order.js`). `history_dashboard_client.js` requires every card to carry
  `data-card-section` and a computed `order` of 0.
- Split-card Graph Bounds menus reuse `main.js`'s shared outside-click handler. Do not add
  per-card document listeners that retain discarded cards. Keep opening and menu interactions
  from dismissing the dropdown, and preserve dismissal on unrelated bubbling icon clicks.
- Run `./addons/test-frontend-regressions.sh` when changing viewport culling, split-card
  teardown/dropdowns, card placement or Rearrange mode, or the vendored Chart.js version. It
  covers mutation retention, point visibility after trimming, split-card listener/instance
  cleanup and card placement, and drives the real `kula serve` in Chromium to check live
  streaming versus bucketed refreshes and rearranging with real input. These browser
  fixtures require Node.js 22+, Chromium/Chrome and Go. The separate Frontend workflow
  (`.github/workflows/frontend.yml`) runs each with its own timeout; `./addons/check.sh` does not.
- `js/chartjs/chartjs-bundle.min.js` is generated from `addons/chartjs/` by
  `./addons/build-chartjs.sh`; never hand-edit it. CI's `chartjs-bundle` job rebuilds it from the
  lockfile and fails when the committed file differs. It registers only the line controller,
  line/point elements, linear/time scales, Legend, Tooltip and zoom, with a native-`Date` time
  adapter. Any other chart type, scale or plugin (e.g. Filler for `fill`) must be added to
  `addons/chartjs/entry.js` and rebuilt. `TestChartBundle` pins the registered set. A distro
  build swaps the bundle for the upstream UMD trio with
  `addons/packaging/replace_chartjs.sh`, which rewrites the template, the fixtures and those
  tests accordingly — the "never hand-edit" rule applies to this repo, not to that helper.
- npm is used only through `addons/build-chartjs.sh` (or by hand in `addons/chartjs/`, whose
  `.npmrc` applies the same guards): `ignore-scripts=true`, `min-release-age=14`, exact pins.
  `verify-lock.js` rejects a lockfile older than v2 (no `packages` map), a direct dependency
  of any kind that is not an exact pin or not locked, and any locked package whose `resolved`
  and sha512 `integrity` are not exactly the registry's `dist.tarball` and `dist.integrity`
  for its name and version, or that is under 14 days old. `addons/chartjs/verify-lock.test.js`
  checks it offline against crafted lockfiles. Do not bypass it or add npm dependencies
  elsewhere.
- Dashboard modules keep preferences through `js/app/prefs.js`, never `localStorage` directly:
  blocked site data makes every access throw, which would stop the dashboard.
  `TestDashboardStorageGoesThroughPrefs` enforces it; only the import-free `tv-mode.js` guards
  its own calls.
- `/api/history` metadata is a contract with the dashboard's live-refresh and aggregation logic.
  `TestHistoryResponsesMatchDashboardContract` (storage) feeds real responses to the frontend
  modules; add a scenario there when a change alters tiers, steps, clipping or extrema profiles.

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
| Chart.js bundle contents, time-scale date adapter | `addons/chartjs/`, then `./addons/build-chartjs.sh` |
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
