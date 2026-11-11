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
| [`aggregation_codec.go`](../../internal/storage/aggregation_codec.go) | Record-level contributing-statistics trailer (weighted sums + seconds) |
| [`query_planner.go`](../../internal/storage/query_planner.go) | Fixed source-selection target, epoch-aligned output steps, query downsampling |
| [`history_stream.go`](../../internal/storage/history_stream.go) | Bounded batch reads, buckets merged across batch boundaries |
| [`tier.go`](../../internal/storage/tier.go) | One ring-buffer file: header + records, wrap handling, chronological reads |
| [`tier_index.go`](../../internal/storage/tier_index.go) | Sparse seek index per tier, its `tier_N.idx` sidecar, and tail reads |
| [`record_checksum.go`](../../internal/storage/record_checksum.go) | CRC-32 trailer of each record: sealing, verification, resync checks |
| [`tier_snapshot.go`](../../internal/storage/tier_snapshot.go) | Consistent tier copies for backups, without stalling the writer |
| [`codec.go`](../../internal/storage/codec.go) | The binary record format (see [Codec](06-storage-codec.md)) |
| [`disk_identity_codec.go`](../../internal/storage/disk_identity_codec.go) | Persistent disk IDs appended after the power-supply section |
| [`tcp_ext_codec.go`](../../internal/storage/tcp_ext_codec.go) | TCP extension (retransmit rate) appended after the disk IDs |

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
- Tier 0's resolution must be an officially supported value (1s, 2s, 5s, 10s, 15s, 30s);
  sub-second values are accepted for testing only.

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

Windows are **aligned to the Unix epoch** — whole minutes for tier 1, whole five minutes for
tier 2 — not counted from process start, so every native rollup is exactly one of the
epoch-aligned output buckets history queries use. A rolling view (native records) and a past
window (which re-buckets records to clip its edge) then show the same minutes with the same
values; with process-aligned windows the two split every interval differently, and a short
spike could read up to half as high in one as in the other. A raw sample whose interval
straddles a window boundary (they usually do, by a fraction of a second) is split there like a
history bucket fragment: its duration and sparse mean weights are apportioned, its values and
extrema go to both parts. A window is flushed as soon as a part ends exactly on its end, or,
after an outage, when a record of a later window arrives. A run cut short inside its window
(a stall of more than two intervals, or an outage) is written as soon as the next record shows
it ended, so a buffer holds at most one run however often collection stalls.

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
that identity exists, so appearance, disappearance, or reordering cannot invent a zero. Disks
identify by their persistent sysfs ID and use the kernel name only as a fallback, so a renamed
device cannot merge two physical drives.

Cascading rollups reuse inner envelopes and preserve effective contributing weights per
metric. Sparse `MeanStats` entries contain a weighted sum and contributing seconds when a
member/reading is absent or an integer mean was rounded. Keys use JSON field names and quoted
dynamic identities. Fully observed, unrounded fields use the bucket duration without an entry.
This preserves fractional integer means across rollups without changing the public metric types.
Floating-point values remain subject to normal arithmetic and float32 codec precision.

Both tests are tolerant, or the statistics stop being sparse. A field's weight is its
bucket's when the two agree to a relative `1e-9` (`meanWeightTolerance`): the field's is a
float sum of seconds per input, the bucket's comes from integer nanoseconds, and on real
collection intervals the two differ in their last bits even when every input contributed.
Compared exactly, most rollups of a real host carried an entry for every mean field and the
trailer outweighed the metrics; mock data, spaced exactly one second apart, never showed it.
A mean counts as rounded only beyond float32 precision (`meanRoundingTolerance`, 2⁻²⁴
relative), the rounding every float mean takes in the codec anyway: the integer mean of a
byte count needs no entry, that of a connection or process count does.

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

`HistoryResult.valid_aggregations` retains the strict response-wide contract for existing
clients. New complete envelopes carry `flagReducerV2`; any raw or legacy bucket restricts
that list to `data`. A native-resolution reduction performed only to clip a right-boundary raw
record still represents raw observations and keeps `data`-only availability, so Min/Max
availability cannot depend on lookahead timing. Compatibility-aware clients use
`available_aggregations` (the union of
available choices), response `extrema_profiles`, and each bucket's `extrema_profile`.
Profiles are `current` (all fields, `*`), `legacy`/`mixed` (an exact scalar allowlist), and
`none` (no extrema). The allowlist in `history_compatibility.go` covers CPU total usage,
user/system/I/O-wait/steal, load averages, used memory and used swap, including their used
percentages. Copied gauges, temperature sentinels, devices and applications are excluded.

The read-time layer makes no record-format changes. Only binary envelopes predating the
mean-statistics and disk-ID extensions qualify as legacy. Incomplete modern envelopes and
old JSON records fail closed; a missing reducer flag alone cannot certify them. Query
fragments and batch merges propagate the intersection of input capabilities, including
`none` from lossy coarse Data-only records. Legacy rounded means cannot expand their stored
extrema. Mixed means remain approximate. The query-only profile is never serialized into
tier records or used to set the reducer flag. See [Binary Codec](06-storage-codec.md).

## Record frame and unclean shutdowns

A tier file is a 64-byte header followed by records, each `[length u32][payload]`. The
payload's first byte selects its format:

| Kind | Payload | Written by |
|------|---------|------------|
| `0x03` | encoded sample, then a CRC-32 trailer | 0.22.0 and later |
| `0x02` | encoded sample | before 0.22.0 |
| `{` | JSON | codec v1, migrated to binary on open |
| anything else | encoded sample, no kind byte | early binary builds |

Kula does not fsync every write. After a power loss or a kernel crash the header can reach the
disk while records it counts do not, or reach it only in part: a page of a record then reads
back as zeros, or as what an older ring pass left there. A process crash cannot do this, because
the header that makes a record visible is written after it. Readers handle the damage two ways:

- **Holes.** In the active segment, a length prefix that cannot start a record makes the scan
  resync to the next plausible record instead of stopping there. A record of a v2 tier starts
  four bytes before its kind byte, so resync passes the positions before the next `0x02` or
  `0x03` at once, and a hole of zeros at the speed of a byte search. A `0x03` candidate must
  match its checksum; an older one must be followed by a second plausible header or end the
  segment exactly. A candidate running past the segment end is never accepted. Checksums that
  fail cost a walk of a segment at most 64 MiB plus 16 bytes for every byte resync passes
  (`resyncBudget`); a candidate the budget cannot pay for is passed over unverified. Real
  damage holds few such candidates, but a crafted file can repeat one every five bytes, each
  claiming nearly a megabyte: verified one by one, 8 MiB of it took 90 s to scan, holding the
  tier lock. Bounded, it takes 0.2 s, and the walk still finds the records after it.
- **Torn records.** The CRC-32 (IEEE, as Python's `zlib.crc32` computes it) covers the length
  prefix and every payload byte before the trailer. A record that fails it is skipped by every
  reader, like any record that does not decode: history scans, `ReadLatest`, restart recovery
  and the read-only store. A torn record therefore never reaches the latest-sample cache or the
  rollups that recovery replays. Records written before 0.22.0 cannot be verified; the ring
  replaces them within one pass.
- **The previous pass.** A wrapped ring's header records where the previous pass starts. On
  open, the record there must prove itself as a resync candidate does, not by its time: after
  the clock steps back, the whole previous pass is later than the newest record. One that does
  not is resynced forward from, never replaced by the write offset, which is no record
  boundary. The header can also lag the data: records a crash left uncounted lie past the
  write offset over the start of the previous pass, followed by the remains of a record they
  overwrote. Without a sidecar from a clean shutdown, opening a tier skips those records
  (checksummed, newer than the header's newest, in order), resyncs past the remains, and walks
  the previous pass to its end, resuming after any record that is not one
  (`verifyOldSegment`). A zero length still ends the pass: it is the sentinel, or a hole no
  reader can tell from one.
- **The header's geometry.** A ring size that is not positive or exceeds 2^62 bytes
  (`maxTierData`), or a write offset outside the ring, is no header any build writes: the
  owner refuses the file and asks for it to be moved aside, and a read-only view and
  `inspect_tier.py` refuse it too. Such an offset near 2^63 once overflowed `Write`'s wrap
  check, now made on the room left in the ring: the record went to that offset, and the next
  scan walked the 9.2 EB sparse file it made, holding the tier lock. A write offset past the
  end of the file is what a crash leaves when the header reached the disk and the records it
  counts did not: readers clip the ring to the file, and the owner resumes writing where the
  data ends instead of past a hole as long as the offset claims.

The check runs only on records a query decodes. It costs about 5 % of a record's decode time
where Go's `hash/crc32` uses CPU instructions (amd64, arm64) and about 25 % with its portable
code (riscv64). Each record grows by four bytes.

## Restart recovery

On startup the store:

1. Opens each tier file and reads its header (version, write offset, wrapped flag), then
   adopts its **seek index** from `tier_N.idx` or rebuilds it with one walk of the ring (see
   below).
2. Reads tier 0's newest `4 × ratio1` records once: the last one restores the
   **latest-sample cache** so `/api/current` works immediately, and those written after the
   record tier 1's newest rollup ended on are replayed into the tier-1 windows, that record
   clipped to its unrolled part when it straddles a window end. Tier 1's newest `4 × ratio2`
   records restore tier 2 the same way, first, so a window the replay finds complete — a
   crash between the raw write that closed it and its rollup — is rolled up then and cascades
   as it would have. Pending records are told apart by write order, not time: after the clock
   steps back, the tail holds records later than the newest rollup that were rolled up before
   the step and pending ones earlier than it (`pendingRollup`). A raw record that claims more
   than twice tier 0's resolution is replayed as covering one, as `WriteSample` writes a
   record after a gap (`rollupSource`): records written before 0.20.0 carry their whole gap,
   and a crafted file any duration. Taken at its word, one claiming a year was split into a
   part per minute of that year, which allocated 8.9 GB and overwrote tier 1. History queries
   bound such a record the same way (`historySourceWidth`), so it fills no bucket it did not
   observe and is not split into a piece for every bucket of the window. With the seek
   index each of these reads only the end of its tier, so rollups resume after a restart
   without double-counting or gaps.

## Seek index

Records are length-prefixed and chronological within a ring pass but carry no back links, so
without help a query reads every record from the oldest one on just to reach its `from`, and
finding the newest records means walking the whole ring. `tier_index.go` keeps, about every
64 KiB of a pass, a record boundary with that record's timestamp and the greatest timestamp
written before it in the same pass. A scan starts at the last point whose predecessors all
precede `from`, so it skips exactly the records a scan from the segment start would have skipped
and returns identical results, even across a clock step backwards (the prefix maximum, not the
point's own timestamp, decides). Each point is confirmed against the file — a valid length
prefix and its exact nanosecond timestamp — before a scan or a tail read trusts it; otherwise
the walk starts at the segment start. `Tier.Write` maintains the index: wraps and overwritten
records drop points, every new record may add one. Anything else that writes records into an
open tier (test helpers simulating older writers) must call `seekAppend` the same way.

A scan stops at the first record past `to`, which is right only while no later record of the
pass has an earlier timestamp. After the clock steps back, some does, and a window ending before
the pre-step peak holds records on both sides of that first record. So the index also keeps,
for the current and the previous pass, the newest **backstep** (a record written below the
pass's greatest timestamp so far) and the pass's greatest timestamp. A scan stops early only at
or past its pass's backstep, and skips the previous pass only when that pass's greatest
timestamp precedes `from` (its first record cannot tell: after a step, the current pass may
begin before the previous one ends). A pass without a backstep scans exactly as before; one
with a backstep reads up to it until the ring overwrites it. Without an index (a legacy tier),
a scan reads every record of its segments.

A clean `Close` saves the index, with both passes' greatest timestamps and backsteps, next to
the tier as `tier_N.idx` (magic `KULAIDX2`; written to a temporary file and renamed; CRC-32
protected; `OpenTier` deletes a temporary file a crash left behind). `OpenTier` adopts it only
when its fingerprint — ring size,
write and oldest offsets, record count, codec version, newest timestamp, file size and wrap
state — equals the tier just opened, so a crash, a restore from backup, a resize or another
binary writing the file all fall back to rebuilding it with one walk. The tier file format is
unchanged and older versions ignore the sidecar.

Read-only stores (the terminal UI, reading what `kula serve` writes) cannot use the sidecar —
the owner's header moves on with every record — so each read-only tier builds its index with
one walk on its first scan and, before each later scan, extends it over the records the owner
appended since (`prepareReadOnlySeek`). A refresh that shows the owner wrapped, or a replaced,
resized or rewritten file, drops it for a rebuild: the view has not walked what the owner
appended to the finished pass since the last refresh, which may step back in time. The owner writes in ring
order and so overwrites the newest point before anything indexed after it; when that point no
longer holds its record, the owner lapped the ring unseen between two refreshes and the index
is rebuilt. Scans the owner overtakes are still caught by `readOnlyScan`.

On 36 hours of mock history at the default tier sizes (223 MB tier 0), a clean restart reads
0.4 MB instead of 460 MB, a restart without sidecars 240 MB, and a history query reads
0.05–6 MB depending on its window instead of the whole 223 MB tier file.

## Query path

`QueryRange(from, to)` / `QueryRangeWithMeta(from, to, points)`:

1. Classify retained tiers by whether they cover both requested edges. The retention-edge
   tolerance is one tier resolution; the live-edge tolerance is two resolutions to account for
   bucket-end timestamps, rollup delay, and normal collection jitter. A coarse tier reaches the
   live edge as far as its rollups are final: its newest rollup, or the start of the window that
   holds the source tier's newest record when that is later, since every earlier window has
   closed. After a restart that followed a downtime, tier 1 therefore keeps covering the live
   edge, its last rollup being the partial minute before the shutdown, instead of yielding to
   tier 2 until the first minute after the restart is rolled up. The retention-edge tolerance
   assumes the oldest rollup spans its whole window; when that tolerance is what makes a coarse
   tier complete, the planner reads the oldest rollup's duration (`Tier.OldestDuration`) and
   counts from where it really starts. A store's first rollups hold only the seconds it ran
   for, so on a fresh start a 5-minute view stays on raw data (partial) instead of becoming one
   5m point.
   A window ending in the future is classified, and its read estimated, as ending now
   (`planningEnd`; after a backwards clock step, at the newest record when that is ahead of
   the clock). Nothing can cover the part that has not happened, so taken as asked every such
   window would be partial on every tier.
2. Prefer the finest fully covering tier whose estimated read fits the fixed **7,500 source
   record** target. This target is independent of `points`, so requesting a wider canvas cannot
   unexpectedly unlock a different tier. A denser tier is skipped only when another fully
   covering tier exists. If no tier covers the full window, return the tier with the greatest
   overlap and mark the result `complete: false`. The target applies there too
   (`preferBudgetedPartial`): the greatest overlap is usually the raw tier's, by the seconds
   its rollups trail it, so a week asked of a two-day-old store would read every raw record.
   A partial tier past the target yields to the finest coarser tier that covers as much of the
   window, give or take that tier's edge tolerances. A dense tier that alone retains more of
   the window is still read: the target picks a source, it never hides data.
   A tier hint (`QueryRangeFromTier`) puts the named tier first only while the part of the
   window it retains fits the target. The hint is a request parameter; the tail of a view
   fits, a month named onto the raw tier does not and is planned as if unhinted.
3. Read records in ring order (handling buffer wrap), starting each segment at its seek
   point for `from` and stopping at the first record past `to` unless a backwards clock step
   follows it (see the seek index above), in batches of at most 512 decoded records. Reduce batches into stable output buckets, preserving contributing counts,
   sparse mean weights, and extrema across batch boundaries. Binary records outside the
   window are skipped using their timestamp headers without allocating metric payloads.
   A scan takes its 1 MiB read buffer from a pool (`scanReaders`) rather than allocating one
   per segment, which for a scan of a few records was nearly all it allocated.
4. Choose a stable output step from 1/2/5-style intervals (including 10, 15, and 30 units),
   align groups to Unix-epoch boundaries, and reduce to at most `points` results. The API caps
   `points` at 5000 and all ranges at 31 days.
5. Serve a repeat of an identical query from a short-lived in-memory **query cache**. Keys are
   the exact nanosecond bounds, point budget and tier hint, not rounded seconds or bucket
   boundaries; an entry lives one tier-0 resolution, and every raw write drops entries whose
   range reaches the live edge. In practice it serves concurrent identical requests and repeats
   of a past window. Dashboard refreshes end at the current time, so each has its own key and
   almost never hits. The cache holds at most 256 entries and 2000 samples across them
   (`maxQueryCacheSamples`): a result of more samples is not cached, and when an entry would
   exceed either bound, expired entries are swept first and, failing that, it is not cached.
   Every distinct request makes its own entry, so the entry cap alone would let a client
   varying the end of a long window keep one multi-megabyte result per request.
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

## Read-only access

`OpenReadOnly(cfg)` opens the configured tier files for queries without owning them. The TUI's
History view uses it to chart what a separately running `kula serve` writes.

- Files are opened `O_RDONLY`, and only as regular files: a symbolic link or a FIFO in place of
  a tier is an error, not followed or waited on (`openRegular`). Nothing is created, migrated,
  resized or rewritten, and a missing directory or tier reads as empty. `WriteSample`,
  `Tier.Write` and `Tier.Flush` return `ErrReadOnly`, and `Close` only releases descriptors.
- The owner rewrites each tier header after every record. `Refresh()` re-reads those headers,
  clears the query cache, and reopens a tier whose file was replaced, for example moved aside
  before a restart. It is a no-op on an owning store.
- Refresh mirrors `Write`'s pass bookkeeping (`writeCycle`, `writeEnd`), so the batched scanner's
  snapshot check still notices if a refresh between batches shows the writer overtaking unread
  bytes.
- Tier locks do not cross processes. A checksummed record the owner overwrites while it is read
  fails its checksum, but a record written before 0.22.0 could decode into wrong values, and a
  torn length prefix misplaces every record after it. Each scan therefore checks itself
  (`readOnlyScan`). The owner writes from the snapshot's `writeOff` in ring order over the oldest
  records, and its header `count` only grows, so re-reading the header shows exactly which bytes
  it has changed since the snapshot. Every 256 records, before a batch is reduced and at the end,
  the scan re-reads the header. If the owner reached a record read since the previous check, that
  record may be torn, and so may every record after it, because each length prefix positions the
  next. The scan then fails with `errHistorySnapshotExpired`. `QueryRangeWithMeta` re-adopts the
  header and retries up to three times. Records that passed a check stand, and when the owner did
  write, samples next to its following write are dropped. The owner writes once per collection
  interval, far slower than a scan, so retries are rare. Treat results as a view, not an archive.
- A length prefix above `maxRecordBytes` (64 MiB), or one reaching past the end of the file, is
  corruption: a torn or corrupt prefix never allocates up to the ring size.
- There is no latest-sample cache, so `QueryLatest` returns `nil`.
- Tier choice and resolution labels come from the config the store was opened with; the files
  do not record resolutions. `LayoutMismatch()` compares what the files do record, each tier's
  size and the number of tier files, and the TUI shows the difference.

`readonly_test.go` covers following a writer across wraps, reading while the owner writes from
another store (every returned value must be one the owner wrote, in order), missing and replaced
files, permission errors, and the guarantee that a read-only store leaves the file byte-for-byte
unchanged, including a legacy JSON tier and a config naming a different tier size.

## Backup snapshots

`Tier.SnapshotTo` ([`tier_snapshot.go`](../../internal/storage/tier_snapshot.go)) gives the
backup scheduler a byte-for-byte copy of a tier that reopens with `OpenTier`, without stopping
the writer. Under the read lock it notes the writer's offset and ring pass; it then copies the
file without the lock; and under the read lock again it copies the header and the ring range
written meanwhile, from the noted offset to the current one (through the end of the ring and on
from its start if it wrapped). `Write` changes no other byte, so the copy equals the file at that
moment. A writer that laps the ring during the copy, possible only for a tiny ring, makes it copy
the whole file again under the lock. Held only for those moments, the lock no longer stalls
`WriteSample` (and with it the store lock, queries and the live stream) for the whole copy:
for a 249 MB tier 0 on NVMe the longest write during a backup went from 100–235 ms to 2 ms.
`snapshot_test.go` writes between the copy and the patch in each way the writer can move and
requires the copy to equal the file.

## Migration

The tier format is versioned. `tier.go` supports migrating **v1 (JSON records)** to **v2
(binary records)**, and the binary codec reads every older record: records written before a new
metric type existed are decoded correctly because absent sections are gated by preamble flags
and skipped. The reverse does not hold; an older binary may misread a newer record. See
[Codec](06-storage-codec.md#reading-older-records--the-core-invariant) and `migration_test.go`.

## Inspection

- `kula inspect` ([cmd/kula/main.go](../../cmd/kula/main.go) → `InspectTierFile`) prints per-tier
  version, fill %, record count, oldest/newest timestamps, wrap flag, time range, and a coverage
  estimate. `--verbose` additionally prints the latest recorded metrics of each tier
  (`InspectLatestTierSample`).
- [`addons/inspect_tier.py`](../../addons/inspect_tier.py) is a standalone Python decoder of the
  same format — useful for offline analysis and as a cross-check on the Go codec. It walks the
  ring as the Go reader does: the same header checks, wrap state and tail validation, segments
  clipped to the file, the 64 MiB record cap, and resync past holes in the active pass with the
  same checksum budget, so after an unclean shutdown it reports the same latest record as
  `kula inspect --verbose`.
  `TestInspectorWalksTheRingLikeTheReader` compares the records both visit on damaged tiers, and
  `TestOpenTierRefusesOffsetsOutsideTheRing` the headers both refuse.

## Mock data

[`cmd/gen-mock-data`](../../cmd/gen-mock-data/main.go) generates deterministic, correlated
multi-day timeseries to stress storage performance and exercise tier rollups, counter resets,
dynamic metric identities, and wrap behavior at scale. Its default profile includes labeled
incident windows; see [Testing & QA](12-testing.md#realistic-mock-history) for usage and profiles.

## Tests & benchmarks

`store_test.go`, `aggregation_test.go`, `tier_test.go`, `codec_test.go`, `snapshot_test.go`,
`migration_test.go`, `tier_header_test.go`, plus `codec_fuzz_test.go`, `FuzzOpenTier` (every
path that opens a tier file, on crafted files) and the benchmark suite
(`./addons/benchmark.sh`). See [Testing](12-testing.md).

Next: [Binary Codec](06-storage-codec.md).
