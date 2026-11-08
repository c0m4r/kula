# Binary Codec

File: [`internal/storage/codec.go`](../../internal/storage/codec.go)

The codec is the on-disk record format for the storage tiers. It is a **positional** layout —
no keys, no TLV, no length-prefixed fields. Metrics are identified solely by their byte offset
within a fixed, deterministic sequence. This makes records dense and fast to encode/decode, at
the cost of requiring strict discipline when adding new fields (see [Adding a Metric Type](14-adding-metrics.md)).

On disk the first byte of each record's payload (after its 4-byte length prefix) tells four
kinds of record apart (`Tier.readRecord`, `extractTimestamp`, `Tier.readTimestampAt`):

| First byte | Record | Decoded as |
|---|---|---|
| `0x03` | binary, written since 0.22.0 | the record below, then a CRC-32 trailer ([`record_checksum.go`](../../internal/storage/record_checksum.go)) that must match |
| `0x02` | binary, written before 0.22.0 | the record below, unverified |
| `{` (`0x7B`) | legacy v1 JSON | `decodeSampleJSON` |
| anything else | legacy binary, written before 0.11.0 added the kind byte | the record below from the first byte, unverified |

The timestamp sits right after the kind byte in `0x02` and `0x03` records. A legacy binary record
has no kind byte and starts with the low byte of its nanosecond timestamp, so about 3 in 256 of
them begin with one of the three tags and are misread or skipped; the ring replaces them as it
wraps. Older binaries cannot read `0x03` records, so keep a backup if you may downgrade. See
[Storage Engine](05-storage-engine.md#record-frame-and-unclean-shutdowns).

## Record structure

```
┌──────────────────────────────────────────────────────────┐
│  Preamble (18 bytes)                                      │
│  [0:8]   Timestamp (int64, nanoseconds)                   │
│  [8:16]  Duration  (int64, nanoseconds)                   │
│  [16:18] Flags     (uint16 bitmask)                       │
├──────────────────────────────────────────────────────────┤
│  Fixed block (218 bytes) — CPU, loadavg, memory, swap,    │
│  TCP, process, system and self metrics. Always present    │
│  at the same size (float32 and integer fields).           │
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
│    9. Disk IDs        (version + count + u16-length IDs)  │
│   10. TCP extension   (version + 4B v1: retrans_ps f32)   │
│                                                           │
│       ← NEW sections go HERE, after every existing one.   │
└──────────────────────────────────────────────────────────┘
```

Sections 1–5 have no gate: every binary record carries them. The GPU section (6) is the one
section gated by length instead of a flag: it predates the flags, so a record with fewer than 2
bytes left after the system strings has no GPU entries and ends there, whatever its flags say.
That works only at the very end of a record (in a rollup the next block's bytes follow), so
every later section has a flag or sits inside one.

The fixed block's `user_count` byte holds `0–254`; `255` means unknown (the collector's `-1`,
for hosts with neither utmp nor systemd-logind sessions) and decodes as `-1`. The byte was an
unclamped `uint8` before, so `255` only ever held a count that had wrapped.

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
    flagHasDiskIDs   uint16 = 1 << 12 // persistent disk identities after power supplies
    flagHasTCPExt    uint16 = 1 << 13 // TCP extension (retransmits) after disk identities
    // bits 5–7 and 14–15 are free for new metric types
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
| 12 | `flagHasDiskIDs` | Persistent disk identities, after power supplies |
| 13 | `flagHasTCPExt` | TCP extension (retransmit rate), after disk identities |
| 5–7, 14–15 | — | **Available** — use bit 14 next |

`flagReducerV2` adds no payload bytes. It distinguishes complete policy-reducer envelopes
from older Min/Max blocks. The history compatibility layer additionally exposes an audited
scalar subset for legacy binary envelopes lacking the mean-statistics and disk-ID extensions.
The decoded/query-only `extrema_profile` is never written into the positional codec and never
promotes an old record to `flagReducerV2`. The Python inspector reports the same decoded profile.

## TCP extension

The fixed block has no room for `net.tcp.retrans_ps`, and it cannot grow without breaking every
stored record, so the rate lives in a section of its own
([`tcp_ext_codec.go`](../../internal/storage/tcp_ext_codec.go)), gated by bit 13. It closes each
Data/Min/Max variable block: `version:u8` (`1`), then the v1 block `retrans_ps:f32`. Growing it
means a new version with a larger block; an unknown version is a decode error, as for power
supplies and disk IDs. Records written before 0.22.0 lack the flag and decode the rate as `0`,
as they always did; older binaries cannot read the extended rollup records, so keep a backup if
you may downgrade. `TestCodecPersistsEveryAggregatedNumericField` fails for any numeric field
with an `agg` policy that does not survive encode → decode, which is how this gap went unseen.

## Contributing statistics extension

When bit 11 (`flagHasMeanStats`) is set, a record-level extension follows **all** Data, Min and
Max fixed/variable blocks. Existing metric offsets and block sizes are unchanged. Raw records
need no extension. Version 1 contains `version:u8`, `complete:u8`, `count:u32`, followed by sorted
entries: `path_length:u16`, UTF-8 path bytes, `sum:f64`, `weight_seconds:f64`, all little-endian.
Paths include quoted dynamic identities; they are not positional member indices.

The sparse map preserves weights for partially present metrics and sums for rounded integer
means. `complete=0` means legacy inputs lacked their original contributing weights; those means
remain approximate. `flagReducerV2` still describes extrema independently. Readers reject
truncated entries, unknown versions, duplicate paths, invalid numeric statistics, and any byte
after the extension: it must end the record. A later record-level extension therefore cannot
simply follow it. It needs a new version of this extension, or a decoder that finds where this
one ends, and binaries from before that change cannot read such records. The Python
inspector decodes the same extension. New metric sections still append inside each variable
block; this record-level extension follows the final block.

## Reading older records — the core invariant

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

Compatibility runs one way: a binary reads every record written before it, but an older binary
is not guaranteed to read a newer record. It ignores a flag bit it does not know, but it cannot
skip that section's bytes. In a raw record (one Data block, no statistics extension) the new
section sits at the end and is left unread; in a rollup the Min and Max blocks that follow are
read from the wrong offsets. A section version newer than the reader knows fares no better:

| Section | Unknown version |
|---|---|
| PostgreSQL, MySQL | read as the newest layout the reader knows; what follows is misaligned |
| Apache2 | nothing read after the version byte; what follows is misaligned |
| Power supplies, disk IDs, TCP extension, contributing statistics | decode error |

So a downgrade needs the backup taken before the upgrade.

## Two ways a metric type evolves

1. **New metric type** → new flag bit + new section appended **after every existing section**.
   Old records lack the flag and skip it.
2. **New fields on an existing type** → bump that section's **version-tagged presence byte**
   (`0` = absent, `1` = v1/old size, `2`, `3`, … = successively larger blocks). The decoder reads
   the version byte and dispatches to the right block layout. The section's *position* never
   changes, so old records stay valid. PostgreSQL (`56/104/121B`) and Apache2 (`72/100B`) already
   use this; MySQL grew the same way (`56/66/74B` plus its IO-state string).

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
