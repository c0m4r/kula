# Binary Codec

File: [`internal/storage/codec.go`](../../internal/storage/codec.go)

The codec is the on-disk record format for the storage tiers. It is a **positional** layout —
no keys, no TLV, no length-prefixed fields. Metrics are identified solely by their byte offset
within a fixed, deterministic sequence. This makes records dense and fast to encode/decode, at
the cost of requiring strict discipline when adding new fields (see [Adding a Metric Type](14-adding-metrics.md)).

Records are kind-tagged `0x02` for format detection (the migration path distinguishes them from
legacy v1 JSON records).

## Record structure

```
┌──────────────────────────────────────────────────────────┐
│  Preamble (18 bytes)                                      │
│  [0:8]   Timestamp (int64, nanoseconds)                   │
│  [8:16]  Duration  (int64, nanoseconds)                   │
│  [16:18] Flags     (uint16 bitmask)                       │
├──────────────────────────────────────────────────────────┤
│  Fixed block (218 bytes) — CPU, memory, swap, TCP,        │
│  process, self metrics (mostly float32). Always present,  │
│  always the same size.                                    │
├──────────────────────────────────────────────────────────┤
│  Variable section — sequential, order is the contract:    │
│    1. Network interfaces (count + per-iface)              │
│    2. CPU temp sensors  (count + per-sensor)              │
│    3. Disk devices      (count + per-device)              │
│    4. Filesystems       (count + per-fs)                  │
│    5. System strings    (hostname, clocksource)           │
│    6. GPU entries       (count + per-GPU)                 │
│    7. Application metrics — fixed sequence:               │
│         a. Nginx       (1B presence + 52B data)           │
│         b. Containers  (2B count + variable)              │
│         c. PostgreSQL  (1B version + versioned data)      │
│         d. MySQL       (1B version + versioned data)      │
│         e. Apache2     (1B version + 72/100B)             │
│         f. Custom      (2B group count + variable)        │
│    8. Power supplies  (version + count + variable)        │
│                                                           │
│       ← NEW sections go HERE, after every existing one.   │
└──────────────────────────────────────────────────────────┘
```

## Preamble flags

```go
const (
    flagHasMin     uint16 = 1 << 0  // min aggregate block present
    flagHasMax     uint16 = 1 << 1  // max aggregate block present
    flagHasData    uint16 = 1 << 2  // representative data block present
    flagHasApps    uint16 = 1 << 3  // application section present
    flagReducerV2  uint16 = 1 << 4  // exhaustive reducer provenance
    flagHasApache2 uint16 = 1 << 8  // Apache2 block present
    flagHasMysql   uint16 = 1 << 9  // MySQL block present
    flagHasPSU     uint16 = 1 << 10 // power-supply section present
    flagHasMeanStats uint16 = 1 << 11 // record-level contributing statistics
    // bits 5–7 and 12–15 are free for new metric types
)
```

| Bit | Flag | Meaning |
|-----|------|---------|
| 0 | `flagHasMin` | Min aggregate present |
| 1 | `flagHasMax` | Max aggregate present |
| 2 | `flagHasData` | Representative Data block present |
| 3 | `flagHasApps` | Application section present |
| 4 | `flagReducerV2` | Min/Max use the exhaustive policy reducer |
| 8 | `flagHasApache2` | Apache2 block present |
| 9 | `flagHasMysql` | MySQL block present |
| 10 | `flagHasPSU` | Power-supply section present |
| 11 | `flagHasMeanStats` | Trailing contributing statistics |
| 5–7, 12–15 | — | **Available** — use bit 12 next |

`flagReducerV2` adds no payload bytes. It distinguishes new trustworthy envelopes from
incomplete Min/Max blocks already present in older tier files, allowing the history API to
advertise operation validity without invalidating or rewriting retained history.

## Contributing statistics extension

When bit 11 (`flagHasMeanStats`) is set, a record-level extension follows **all** Data, Min and
Max fixed/variable blocks. Existing metric offsets and block sizes are unchanged. Raw records
need no extension. Version 1 contains `version:u8`, `complete:u8`, `count:u32`, followed by sorted
entries: `path_length:u16`, UTF-8 path bytes, `sum:f64`, `weight_seconds:f64`, all little-endian.
Paths include quoted dynamic identities; they are not positional member indices.

The sparse map preserves weights for partially present metrics and sums for rounded integer
means. `complete=0` means legacy inputs lacked their original contributing weights; those means
remain approximate. `flagReducerV2` still describes extrema independently. Readers reject
truncated entries, unknown versions, duplicate paths, and invalid numeric statistics. The Python
inspector decodes the same extension. New metric sections still append inside each variable
block; this record-level extension follows the final block.

## Forward compatibility — the core invariant

A new application metric type is gated by a **dedicated preamble flag bit**. The decoder checks
each flag in sequence; if a flag is absent (an old record written before that type existed),
the decoder **skips that section's bytes entirely**, keeping every subsequent section correctly
aligned.

Example — a record written before Apache2 existed (`hasApache2 = 0`):

```
decodeVariable(hasApps=true, hasApache2=false, ...):
  nginx:       read 1 byte  → done
  containers:  read 2 bytes → done
  postgres:    read 1 byte  → done
  mysql:       read 1 byte  → done
  apache2:     flag FALSE → skip this section entirely   ← stays aligned
  custom:      read 2 bytes + groups → done
```

The order is deterministic, so the decoder always knows which bytes belong to which section. A
missing flag means "pretend this section doesn't exist and move on."

## Two ways a metric type evolves

1. **New metric type** → new flag bit + new section appended **after every existing section**.
   Old records lack the flag and skip it.
2. **New fields on an existing type** → bump that section's **version-tagged presence byte**
   (`0` = absent, `1` = v1/old size, `2` = v2/new size). The decoder reads the version byte and
   dispatches to the right block layout. The section's *position* never changes, so old records
   stay valid. PostgreSQL (`56/104B`) and Apache2 (`72/100B`) already use this.

> **The Rule:** never insert a new section *between* existing ones, and never reuse a flag bit.
> Append-only, flag-gated, version-tagged.

## Cross-language parity

The Python decoder [`addons/inspect_tier.py`](../../addons/inspect_tier.py) re-implements this
exact layout (same flag constants, same section order). Any codec change must be mirrored there,
or it will mis-decode records. It serves as both an offline tool and a parity check.

## Tests

- [`codec_test.go`](../../internal/storage/codec_test.go) — round-trip encode/decode, version
  dispatch, old-record skip behavior.
- [`codec_fuzz_test.go`](../../internal/storage/codec_fuzz_test.go) — `FuzzDecodeSample`, with a
  seed corpus under `testdata/fuzz/`. The decoder must never panic on malformed input.
- `migration_test.go` — v1 → v2 migration.

## Adding a metric type

The complete 14-step checklist (config → types → collector → orchestrator → sandbox → codec
flag → encode → decode → aggregation → Python decoder → frontend → docs → tests → verify) is in
[Adding a Metric Type](14-adding-metrics.md).

Next: [Web Server & API](07-web-server-api.md).
