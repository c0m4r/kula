# Testing & QA

Kula has unit tests, race tests, native fuzz targets, benchmarks, in-process runtime security
tests, and an out-of-tree black-box scanner. The canonical gate is `./addons/check.sh`.

## The check gate

```bash
./addons/check.sh
```

Runs, in order (all must pass):

1. `govulncheck ./...` — known-vulnerability scan.
2. `gofmt -l .` — fails on any unformatted file.
3. `go vet ./...`.
4. `go test -v -race ./...` — full suite with the race detector.
5. `golangci-lint run ./...`.

CI runs the equivalent ([`.github/workflows/ci.yml`](../../.github/workflows/ci.yml)) plus
Semgrep ([`semgrep.yml`](../../.github/workflows/semgrep.yml)); CI itself has no standalone
`gofmt` step.

## Unit tests

Notable suites:

| Area | Tests |
|------|-------|
| Collectors | `cpu_test.go`, `disk_test.go`, `network_test.go`, `memory_test.go`, `process_test.go`, `system_test.go`, `containers_test.go`, `app_test.go` |
| Storage | `store_test.go`, `tier_test.go`, `codec_test.go`, `snapshot_test.go`, `migration_test.go` |
| Web/security | `auth_test.go`, `server_test.go`, `websocket_test.go`, `ollama_test.go`, `prometheus_test.go`, `runtime_security_test.go` |
| Config | `config_test.go` |
| Sandbox | `sandbox_test.go` |
| Backup | `backup_test.go`, `cron_test.go` |
| i18n | `i18n_test.go` |
| TUI | `tui_test.go` |

Collectors are driven against synthetic `/proc` and `/sys` fixture trees under
[`internal/collector/testdata/`](../../internal/collector/testdata/), so they run deterministically
on any machine. The sandbox tests are *negative* tests — they assert that forbidden
writes/exec/network actually fail once Landlock is enforced.

When Node.js is available, `server_test.go` runs `history_frontend_test.mjs`. It checks request
cancellation and stale responses, envelope preservation, formatter reuse, missing-data breaks,
viewport batching and culling, gestures, keyboard exploration and complete formula-safe CSV.
Storage tests cover sparse contributing statistics through multiple reductions, disk round
trips, malformed metadata, old records and parity with the Python decoder. API tests retain
the 31-day range cap even when more data exists on disk.

## Fuzzing

Go-native fuzz targets (each with a committed seed corpus that also runs under plain
`go test`):

| Package | Target |
|---------|--------|
| `config` | `FuzzNormalizeBasePath`, `FuzzValidateOllamaURL`, `FuzzParseSize` |
| `web` | `FuzzValidateOrigin`, `FuzzGetClientIP` |
| `collector` | `FuzzParseNginxStatus`, `FuzzParseApache2Status`, `FuzzParseUintBytes`, `FuzzCustomMessage`, `FuzzCollectProcessesStat` |
| `storage` | `FuzzDecodeSample`, `FuzzExtractTimestamp`, `FuzzEncodeDecodeRoundTrip` |

Run mutation-based fuzzing across all targets:

```bash
./addons/fuzz.sh            # 30s per target, all targets
./addons/fuzz.sh 2m         # 2 minutes per target
./addons/fuzz.sh -t 2m      # same as the bare duration argument
./addons/fuzz.sh 1m Decode  # only targets matching "Decode"
./addons/fuzz.sh -r 1m      # with the race detector
./addons/fuzz.sh -l         # list discovered targets
```

The decode fuzzers are particularly important: the codec must **never panic** on malformed
on-disk records. Crashers are saved under `testdata/fuzz/`.

## Benchmarks

The storage engine has a benchmark suite:

```bash
./addons/benchmark.sh                 # 3s per bench, single pass, pretty output
./addons/benchmark.sh 5s              # longer for tighter numbers
./addons/benchmark.sh -c 5 -o new.txt # 5 runs, benchstat-compatible output
benchstat old.txt new.txt             # compare two runs
```

`BenchmarkAggregation` isolates policy reduction from storage and caching, using both raw
60-sample windows and cascades of five rollups. `BenchmarkDownsampling` includes minimal and
all-section metric fixtures, each with separate `Cold` and `Cache` cases. Cold cases clear
the query cache outside the timed region; cache cases warm the result and extend its TTL so
they measure result cloning consistently. The older repeated `BenchmarkQueryRange_Small`,
`Large`, and `Wrapped` cases mostly measure cache hits.

### Realistic mock history

[`cmd/gen-mock-data`](../../cmd/gen-mock-data/main.go) writes the complete metric schema into
the configured tier store. It uses `collection.interval`, so generated timestamps and storage
aggregation ratios remain consistent when testing a non-default collection rate.

```bash
# Repeatable six-hour fixture in an isolated directory.
KULA_DIRECTORY=/tmp/kula-mock go run ./cmd/gen-mock-data \
  -config config.example.yaml -duration 6h -yes \
  -seed 1263881281 -start 2026-09-07T00:00:00Z

# Seven days ending near now, with normal workload variation but no faults.
KULA_DIRECTORY=/tmp/kula-steady go run ./cmd/gen-mock-data \
  -config config.example.yaml -days 7 -profile steady -yes
```

The default `realistic` profile combines weekday/day-night traffic, nightly backups, thermal
lag, memory/cache pressure, filesystem growth, monotonic counters, and correlated application
activity. It also schedules and prints the exact windows for a traffic surge, disk saturation,
memory leak, rolling deployment, packet loss, replication lag, mains outage, and reboot. These
transitions exercise min/max rollups, counter resets, absent metric sections, persistent disk
IDs, container identity changes, and variable record sizes. `-profile steady` retains the
ordinary correlated workload but omits those fault transitions.

When `applications.custom` contains chart definitions, the generator mirrors those group and
metric names and chooses bounded, workload-correlated values using their names, units, and
configured maxima. With no custom definitions it still encodes built-in request-pipeline and
Go-runtime groups for storage/API coverage.

The default seed is fixed. Set both `-seed` and `-start` to reproduce metric values and
timestamps exactly. `-duration` is useful for short fixtures and overrides `-days`; `-yes`
supports unattended runs. The generator does not clear its destination, and the bounded ring
files may replace data already retained there, so use an isolated `KULA_DIRECTORY` for tests.

Two focused history benchmarks are also available:

```bash
go test ./internal/storage -run '^$' \
  -bench '^BenchmarkQueryRange_SourceBudget$' -benchmem -benchtime=3x
go test ./internal/web -run '^$' \
  -bench '^BenchmarkHistoryPayloadEncoding$' -benchmem -benchtime=3x
```

Aggregation schema policies are enforced by `TestAggregationPoliciesCoverSampleSchema` during
the test suite; `go build` alone does not validate aggregation tags. Storage regressions also
cover nested sample ownership under concurrent callers and bounded history decode batches,
including complete 30-day views, sub-resolution observations, wrapped tiers, and writes
between batches. Cross-batch reduction must preserve means, extrema, and contributor counts.

The source-budget benchmark clears the query cache outside the timed region and reads 7,500
raw records. On an AMD Ryzen 5 5600H with Go 1.26.7, compiling reducer plans and reusing field
buffers reduced the median from 112.8 to 41.7 ms/op, 103.2 to 33.4 MB/op, and 660,613 to
101,988 allocs/op (three runs at `-benchtime=5x`). Cold 3,600-record queries measured 54.8 to
20.1 ms/op for the minimal fixture and 244.3 to 77.4 ms/op with all metric sections present.
These are cumulative allocations, not peak live memory. The application-heavy 120-bucket
JSON fixture measured:

| Shape | Encoded size | Parsed JSON nodes | Encode time | Encoder allocation |
|---|---:|---:|---:|---:|
| Full | 5,857,996 B | 356,896 | 14.60 ms/op | 11.79 MB/op |
| Summary sections | 724,087 B | 59,543 | 2.77 ms/op | 1.65 MB/op |

The same section selection is only a 1.1× reduction for the committed bare-host fixture; its
benefit depends on disks, filesystems, GPUs, containers, applications, and custom metrics.
Timing/allocation figures are short diagnostic runs, not production service-level guarantees;
encoder pooling, garbage collection, and hardware affect them.

`TestAggregatedRecordSizeBudget` measures a 60-observation rollup with eight intermittent
containers and sixteen intermittent custom metrics. It currently encodes to **8,190 bytes**,
including 72 sparse mean statistics (4,926 bytes above the same envelope without statistics).
The test enforces a 10 KiB ceiling to make material retention regressions visible. This is not
a universal record-size bound: deployment cardinality and identity-string lengths matter.
At this fixture's size, default 150 MiB minute and 50 MiB five-minute tiers hold roughly 13.3
and 22.2 days respectively, including the four-byte record prefix. Measure real tiers with
`kula inspect` before relying on a specific retention window; fixed-size files do not promise
a fixed number of days.

### Real-browser fixtures

Run the dashboard regression against actual frontend modules and a controlled local API:

```bash
node internal/web/testdata/history_dashboard_test.mjs
```

It requires Node.js 22+, Chromium/Chrome, and localhost access. It exercises the real grid/list transition,
legend and instance preservation, Data opt-in/opt-out, two accelerated hours of rolling
24-hour history, failed refreshes, a 5,000-observation response with extra gap markers,
horizontal time labels, visible sampling tiers, custom-range validation and time zone
conversion, frozen custom ranges, sub-three-hour tier-0 aggregation/zoom, slow collection
cadence, sensor reorder/disappearance, application gaps, and live language switching.
History replay reports median preparation time and enforces a DOM-mutation budget proportional
to chart count. Device-selection checks preserve unrelated datasets and shared gap metadata;
all chart coordinates must remain valid for disabled Chart.js parsing.
Timing is diagnostic; assertions check behavior rather than host-specific
millisecond limits. The fixture uses temporary browser data and cleans it up on exit.

`history_performance.html` separately measures 46 charts with three 1,200-point datasets each.
Run it with a fixed DevTools viewport:

```bash
node internal/web/testdata/history_performance_test.mjs
```

Its JSON result must report
`status: "pass"`, fewer updated charts than registered charts, the same visible count for
crosshair renders, working keyboard gestures, exact UTC tooltips, off-by-default Data controls,
and a lazy 50-row table with CSV after opt-in. Run both fixtures together with
[`addons/test-frontend-regressions.sh`](../../addons/test-frontend-regressions.sh), which needs
Node.js 22+ and a Chromium/Chrome executable and reports the skipped optional local dependency
when either is missing; neither `./addons/check.sh` nor CI runs them. Set `KULA_CHROMIUM` when
the browser lives at a non-standard path.

## Runtime security tests

[`internal/web/runtime_security_test.go`](../../internal/web/runtime_security_test.go) spins up a
real server in-process and probes it (e.g. raw-socket path traversal) — verifying defenses end to
end, not just unit logic.

## Black-box scanning (kula-scan)

[`kula-scan`](13-kula-scan.md) verifies the same defenses **in the field**, over HTTP/WebSocket,
against a running instance — complementing the in-tree tests. It imports nothing from `internal/`,
so every assertion is made over the wire. Use it as a release/CI gate against a stood-up
instance.

## Python helpers

The operator/build Python scripts are checked with:

```bash
black addons/*.py
pylint addons/*.py
mypy --strict addons/*.py
```

Next: [kula-scan](13-kula-scan.md).
