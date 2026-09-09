# Storage Engine

Package: [`internal/storage`](../../internal/storage/)

Kula's storage is a **custom tiered ring-buffer** that writes metric samples directly into
fixed-size binary files. Because each file has a hard maximum size, new data wraps around and
overwrites the oldest entries — bounded disk usage with no external database, no compaction
jobs, and no GC pressure from unbounded growth.

## Files

| File | Role |
|------|------|
| [`store.go`](../../internal/storage/store.go) | The tiered store manager — write path, aggregation, query, caching |
| [`aggregation.go`](../../internal/storage/aggregation.go) | Exhaustive policy reducer, dynamic identity union, cascading rollups |
| [`aggregation_plan.go`](../../internal/storage/aggregation_plan.go) | Immutable schema plans, direct output writes and per-call reduction buffers |
| [`query_planner.go`](../../internal/storage/query_planner.go) | Fixed source-selection target, epoch-aligned output steps, query downsampling |
| [`tier.go`](../../internal/storage/tier.go) | One ring-buffer file: header + records, wrap handling, chronological reads |
| [`codec.go`](../../internal/storage/codec.go) | The binary record format (see [Codec](06-storage-codec.md)) |

On disk, each tier is a file `tier_N.dat` in `storage.directory`.

## Tiers

Three tiers by default, each progressively coarser and smaller:

| Tier | Resolution | Default size | Content |
|------|-----------|--------------|---------|
| Tier 0 (`tier_0.dat`) | 1s | 250 MB | Raw samples (must equal `collection.interval`) |
| Tier 1 (`tier_1.dat`) | 1m | 150 MB | 1-minute metric rollups |
| Tier 2 (`tier_2.dat`) | 5m | 50 MB | 5-minute metric rollups |

### Tier validation

At startup ([config](../../internal/config/config.go)) the tier hierarchy is validated:

- Resolutions strictly ascending (T0 < T1 < T2).
- Each higher resolution divisible by the lower one.
- Ratio between adjacent tiers capped (max **300:1**) to bound aggregation-buffer memory.
- Tier 0's resolution must equal `collection.interval`.

## Write path

```
WriteSample(sample)
   │
   ├─► append-encode into Tier 0 ring buffer (raw)
   │
   └─► feed the aggregation buffer for Tier 1
          when a 1-minute window closes → write a rollup envelope to Tier 1,
          and feed that into the Tier 2 (5-minute) aggregation buffer
                 when a 5-minute window closes → write to Tier 2
```

Each tier keeps an in-memory aggregation buffer accumulating the samples for its current
window. When the window closes, the rollup record is flushed to that tier's file and fed
into the next coarser tier. This cascading rollup is why very coarse resolutions raise memory
use — more samples buffer before each flush.

Flushes partition the buffered observations into contiguous runs. Collection outages produce
separate partial rollups at every tier, preserving each run's contributing duration instead
of moving pre-outage values into a post-outage bucket. Recovery applies the same partitioning
to pending buffers reconstructed from disk; no record-layout change is required.

## Aggregation semantics

Each aggregated record can carry `Data`, `Min`, and `Max` blocks (encoded with the
`flagHasData`/`flagHasMin`/`flagHasMax` preamble flags). Every numeric collector field declares
one policy in its `agg` struct tag:

- `mean` — duration-weighted bucket mean; integer fields round to the nearest representable
  integer;
- `mean_nonnegative` — the same, but negative sentinel values are unavailable and excluded;
- `last` — the latest observation, used for monotonic counters and capacity/configuration
  gauges;
- `identity` / `identity_fallback` — stable keys used to match members of dynamic collections.

Strings and states retain the latest value (strings prefer the latest non-empty value). `Min`
and `Max` contain element-wise, per-series extrema for every numeric metric; they do not
describe one simultaneous system state. Dynamic devices, sensors, filesystems, GPUs, power
supplies, containers, and custom metrics are unioned and reduced only across observations where
that identity exists, so appearance, disappearance, or reordering cannot invent a zero.

Cascading rollups reuse inner envelopes and preserve effective contributing weights per
metric. Sparse `MeanStats` entries contain a weighted sum and contributing seconds when a
member/reading is absent or an integer mean was rounded. Keys use JSON field names and quoted
dynamic identities. Fully observed, unrounded fields use the bucket duration without an entry.
This preserves fractional integer means across rollups without changing the public metric types.
Floating-point values remain subject to normal arithmetic and float32 codec precision.

A trailing `flagHasMeanStats` section persists these statistics after all Data/Min/Max blocks.
`MeanWeightsComplete` is false for a rollup containing legacy buckets: their original missing
weights cannot be recovered. Their stored means remain approximate; their independently trusted
Min/Max envelopes remain usable. A schema-walking test rejects missing numeric policies or
collection identities.

Reducer plans compile the schema's field indices, policies, static statistic paths, and
identity fields once. Reductions write directly into their own output graphs and reuse a
temporary field-value buffer across siblings within each struct. Plans contain no mutable
sample state, so concurrent history queries and rollups share only schema metadata. Dynamic
keys keep the existing persisted `MeanStats` spelling; extrema do not build statistic paths.

`HistoryResult.valid_aggregations` is the authoritative presentation contract. New complete
envelopes carry `flagReducerV2` and advertise `data`, `min`, and `max`. Raw records and legacy
rollups remain readable, but return `data` only; this prevents pre-policy Min/Max blocks already
on disk from becoming trusted merely because Kula was upgraded. See
[Adding a Metric Type](14-adding-metrics.md).

## Restart recovery

On startup the store:

1. Opens each tier file and reads its header (version, write offset, wrapped flag).
2. Restores the **latest-sample cache** so `/api/current` works immediately.
3. Reconstructs any **pending aggregation buffers** so tier rollups resume correctly after a
   restart without double-counting or gaps.

## Query path

`QueryRange(from, to, points)` / `QueryRangeWithMeta(...)`:

1. Classify retained tiers by whether they cover both requested edges. The retention-edge
   tolerance is one tier resolution; the live-edge tolerance is two resolutions to account for
   bucket-end timestamps, rollup delay, and normal collection jitter.
2. Prefer the finest fully covering tier whose estimated read fits the fixed **7,500 source
   record** target. This target is independent of `points`, so requesting a wider canvas cannot
   unexpectedly unlock a different tier. A denser tier is skipped only when another fully
   covering tier exists. If no tier covers the full window, return the tier with the greatest
   overlap and mark the result `complete: false`.
3. Read records chronologically (handling buffer wrap) in batches of at most 512 decoded
   records. Reduce batches into stable output buckets, preserving contributing counts,
   sparse mean weights, and extrema across batch boundaries. Binary records outside the
   window are skipped using their timestamp headers without allocating metric payloads.
4. Choose a stable output step from 1/2/5-style intervals (including 10, 15, and 30 units),
   align groups to Unix-epoch boundaries, and reduce to at most `points` results. The API caps
   `points` at 5000 and all ranges at 31 days.
5. Serve identical recent queries from a short-lived in-memory **query cache** when possible.
   Cache keys preserve the exact nanosecond bounds, not rounded seconds or bucket boundaries.
   Results own their complete sample graphs, including nested maps, slices, and application
   pointers. Copy plans are compiled once from the schema. Input samples and `QueryLatest`
   results use the same ownership isolation.

`QueryLatest()` returns the cached most-recent sample.

`QueryRangeWithMeta` also returns the selected source `Tier`, effective output `Resolution`, the
requested and actual bounds, whether output is downsampled, retention completeness (with live
lag tolerance), and exact edge completeness (without tolerance). Neither completeness flag
asserts that there are no interior gaps. End-timestamped source intervals are apportioned
across overlapping output buckets with proportionate contributing weights; raw intervals use
their observed duration, while coarse rollups use the smaller of contributing duration and
native source resolution. An empty raw query within retained raw bounds is authoritative and
does not fall back to a legacy coarse rollup that might incorrectly cover an outage.
Tier metadata is surfaced in the
`[API History]` perf log line.

The 7,500-record target selects a source tier; it does not reject longer ranges. For example,
30 days at the default five-minute tier requires 8,640 records and remains fully queryable.
Memory scales with one decode batch and the output buckets (or at most `points` native
observations), rather than the total source count. This is a record-count bound, not a bound
on bytes per metric graph or total CPU work. Each batch is reduced outside the tier and store
locks so collection can continue. The scanner fixes its source extent at start, excludes new
appends, and detects overwritten unread ring bytes; an invalidated snapshot is retried once
without publishing partial data. Cache publication checks the write generation so a
concurrent write cannot be followed by insertion of an obsolete query result.
On an AMD Ryzen 5 5600H with Go 1.26.7, the cold-query benchmark at 7,500 raw records measured
about **113 ms → 42 ms**, **103.2 MB → 33.4 MB allocated**, and **660,613 → 101,988 allocations**
per query after compiling reducer plans and reusing field buffers (median of three
`-benchtime=5x` runs). These are cumulative allocations, not peak live memory. Validate latency,
collection-write delay, concurrency, and allocations on supported low-power targets before
increasing the batch size or selection target.

## Migration

The tier format is versioned. `tier.go` supports migrating **v1 (JSON records)** to **v2
(binary records)**, and the binary codec is forward-compatible: records written before a new
metric type existed are decoded correctly because absent sections are gated by preamble flags
and skipped. See [Codec](06-storage-codec.md) and `migration_test.go`.

## Inspection

- `kula inspect` ([cmd/kula/main.go](../../cmd/kula/main.go) → `InspectTierFile`) prints per-tier
  version, fill %, record count, oldest/newest timestamps, wrap flag, and time range.
- [`addons/inspect_tier.py`](../../addons/inspect_tier.py) is a standalone Python decoder of the
  same format — useful for offline analysis and as a cross-check on the Go codec.

## Mock data

[`cmd/gen-mock-data`](../../cmd/gen-mock-data/main.go) generates deterministic, correlated
multi-day timeseries to stress storage performance and exercise tier rollups, counter resets,
dynamic metric identities, and wrap behavior at scale. Its default profile includes labeled
incident windows; see [Testing & QA](12-testing.md#realistic-mock-history) for usage and profiles.

## Tests & benchmarks

`store_test.go`, `aggregation_test.go`, `tier_test.go`, `codec_test.go`, `snapshot_test.go`,
`migration_test.go`, plus `codec_fuzz_test.go` and the benchmark suite
(`./addons/benchmark.sh`). See [Testing](12-testing.md).

Next: [Binary Codec](06-storage-codec.md).
