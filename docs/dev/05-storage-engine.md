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
| [`tier.go`](../../internal/storage/tier.go) | One ring-buffer file: header + records, wrap handling, chronological reads |
| [`codec.go`](../../internal/storage/codec.go) | The binary record format (see [Codec](06-storage-codec.md)) |

On disk, each tier is a file `tier_N.dat` in `storage.directory`.

## Tiers

Three tiers by default, each progressively coarser and smaller:

| Tier | Resolution | Default size | Content |
|------|-----------|--------------|---------|
| Tier 1 (`tier_0.dat`) | 1s | 250 MB | Raw samples (must equal `collection.interval`) |
| Tier 2 (`tier_1.dat`) | 1m | 150 MB | 1-minute Avg/Min/Max aggregation |
| Tier 3 (`tier_2.dat`) | 5m | 50 MB | 5-minute Avg/Min/Max aggregation |

### Tier validation

At startup ([config](../../internal/config/config.go)) the tier hierarchy is validated:

- Resolutions strictly ascending (T1 < T2 < T3).
- Each higher resolution divisible by the lower one.
- Ratio between adjacent tiers capped (max **300:1**) to bound aggregation-buffer memory.
- Tier 1's resolution must equal `collection.interval`.

## Write path

```
WriteSample(sample)
   │
   ├─► append-encode into Tier 1 ring buffer (raw)
   │
   └─► feed the aggregation buffer for Tier 2
          when a 1-minute window closes → write Avg/Min/Max record to Tier 2,
          and feed that into the Tier 3 (5-minute) aggregation buffer
                 when a 5-minute window closes → write to Tier 3
```

Each tier keeps an in-memory aggregation buffer accumulating the samples for its current
window. When the window closes, the Avg/Min/Max record is flushed to that tier's file and fed
into the next coarser tier. This cascading rollup is why very coarse resolutions raise memory
use — more samples buffer before each flush.

## Aggregation semantics

For each aggregated window, the store records three variants per metric — **average**,
**minimum**, and **maximum** (encoded with the `flagHasMin`/`flagHasMax`/`flagHasData` preamble
flags). The web UI's aggregation selector and `web.default_aggregation` choose which variant to
display. Rate fields (per-second metrics) are **averaged** across the window; the same pattern
must be followed for any new rate metric (see [Adding a Metric Type](14-adding-metrics.md)).

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
2. Prefer a fully covering tier that satisfies the source-density budget. If no tier covers the
   full window, return the tier with the greatest overlap and mark the result `complete: false`.
3. Read the records in that range chronologically (handling buffer wrap).
4. **Downsample** to at most `points` results (the API caps `points` at 5000 and the window at
   31 days).
5. Serve from an in-memory **query cache** when possible.

`QueryLatest()` returns the cached most-recent sample.

`QueryRangeWithMeta` also returns the selected `Tier` and `Resolution`, the requested and actual
bounds, and whether coverage is complete. Tier metadata is surfaced in the `[API History]` perf
log line.

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

[`cmd/gen-mock-data/main.go`](../../cmd/gen-mock-data/main.go) generates realistic multi-day
timeseries to stress storage performance and exercise tier rollups/wrap behavior at scale.

## Tests & benchmarks

`store_test.go`, `tier_test.go`, `codec_test.go`, `snapshot_test.go`, `migration_test.go`, plus
`codec_fuzz_test.go` and the benchmark suite (`./addons/benchmark.sh`). See [Testing](12-testing.md).

Next: [Binary Codec](06-storage-codec.md).
