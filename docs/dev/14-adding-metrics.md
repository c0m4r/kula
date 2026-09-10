# Adding a Metric Type

This is the end-to-end checklist for adding a new **application metric type** (e.g. Redis) to
Kula. Because the storage codec is positional and append-only, a new type touches the config,
collector, sandbox, codec (encode + decode), aggregation, the Python decoder, the frontend, and
tests. Read [Binary Codec](06-storage-codec.md) first so the invariants make sense.

The example below adds a type called `Foo` with a 48-byte fixed block.

> Verify at the end with `./addons/check.sh` — all four checks (govulncheck, vet, race tests,
> golangci-lint) must pass.

## 1. Config — `internal/config/config.go`

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

## 2. Types — `internal/collector/types.go`

```go
type FooStats struct {
    MetricA int     `json:"metric_a"`
    MetricB float64 `json:"metric_b"`
}
```

Add `Foo *FooStats` to `ApplicationsStats`.

## 3. Collector — `internal/collector/foo.go`

Implement `collectFoo(elapsed float64) *FooStats`, following the nginx pattern: lazy-allocated
HTTP client, **return `nil` on error**, parse the upstream format. If the source has cumulative
counters that reset on restart, **guard the delta against rollback** (see `nginx.go` /
`apache2.go`).

## 4. Orchestrator — `internal/collector/collector.go`

- Add `fooClient *http.Client` and `prevFoo fooRaw` to the `Collector` struct.
- Add a startup log when enabled: `log.Printf("[foo] monitoring enabled at %s", ...)`.
- Dispatch in `collectApps()`: `if c.appCfg.Foo.Enabled { apps.Foo = c.collectFoo(elapsed) }`.

## 5. Sandbox — `internal/sandbox/sandbox.go`

If the collector makes outbound connections, add a `ConnectTCP` rule for its port:

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

## 6. Preamble flag — `internal/storage/codec.go`

Add a new flag using the next free bit (bit 13):

```go
const (
    flagHasMin       uint16 = 1 << 0
    flagHasMax       uint16 = 1 << 1
    flagHasData      uint16 = 1 << 2
    flagHasApps      uint16 = 1 << 3
    flagReducerV2    uint16 = 1 << 4
    flagHasApache2   uint16 = 1 << 8
    flagHasMysql     uint16 = 1 << 9
    flagHasPSU       uint16 = 1 << 10
    flagHasMeanStats uint16 = 1 << 11
    flagHasDiskIDs   uint16 = 1 << 12
    flagHasFoo       uint16 = 1 << 13  // <-- NEW (never reuse a bit)
)
```

Always set it in `appendPreamble()` — new records always carry the flag:

```go
flags |= flagHasFoo
```

## 7. Encode — `internal/storage/codec.go` (`appendVariable`)

**Append** the new section after every existing section:

```
nginx → containers → postgres → mysql → apache2 → custom → psu → disk IDs → foo
                                                                             ^^^^^
```

```go
// Foo (1-byte presence + 48-byte fixed block when present)
if s.Apps.Foo != nil {
    buf = append(buf, 1)
    f := s.Apps.Foo
    var fb [48]byte
    // binary.LittleEndian.PutUint32(fb[0:], uint32(f.MetricA)) ...
    buf = append(buf, fb[:]...)
} else {
    buf = append(buf, 0)
}
```

When the block grows later, bump the presence tag to `2` and add version-tagged decoding (the
section's *position* must never move).

## 8. Decode — `internal/storage/codec.go` (`decodeVariable`)

Gate the section behind the flag, appended after the disk-ID section — the last section in the
variable block:

```go
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
vn, err := decodeVariable(data[off:], s, hasApps, hasApache2, hasMysql, hasPSU, hasDiskIDs, hasFoo)
```

Update the `decodeVariable` signature and **all call sites (tests included)**.

## 9. Aggregation — `internal/storage/aggregation.go`

Declare the Data reducer for **every numeric field** with an `agg` tag in the collector type:

```go
type FooStats struct {
    Current int     `json:"current" agg:"mean"`
    RatePS  float64 `json:"rate_ps" agg:"mean"`
    Total   uint64  `json:"total" agg:"last"`
}
```

- `mean` computes a duration-weighted mean (`int`/`uint` results are rounded).
- `mean_nonnegative` additionally excludes negative unavailable sentinels such as `-1`.
- `last` retains the newest value and is appropriate for monotonic counters, identifiers, and
  fixed capacity/configuration values.
- `identity` marks the stable key for an element in a dynamic slice.
- `identity_fallback` is used only when the primary identity is empty (for example, a container
  ID when its human-readable name is unavailable).

Min/Max extrema are generated for every numeric metric regardless of whether its Data policy is
`mean` or `last`. Dynamic collections are unioned and matched by identity; never match by slice
position. `TestAggregationPoliciesCoverSampleSchema` fails if a numeric field lacks a policy or
a dynamic collection lacks an identity, so no manual deep-copy or per-application averaging
branch is needed.

## 10. Python decoder — `addons/inspect_tier.py`

- Add `FLAG_HAS_FOO = 1 << 13`.
- Extract `has_foo` from flags, pass to `_decode_variable()`.
- Add the Foo decode block at the same trailing position (after disk IDs), gated `if has_foo:`.
  **This must mirror the Go codec exactly** or it will mis-decode.

## 11. Frontend charts — `internal/web/static/js/app/charts-data.js`

- Add an `APP_ORDER_FOO` constant (increment by 10).
- Create charts dynamically on first data: `if (s.apps?.foo) { ... }`.
- Register the chart card IDs in `charts-init.js` `destroyAppCharts()` for cleanup.

## 12. Config docs — `config.example.yaml`

Add the config section with comments explaining prerequisites (and update the user
[Application Monitoring](../user/08-application-monitoring.md) page).

## 13. Tests — `internal/collector/app_test.go`

- Valid parse output.
- Malformed output → `nil`.
- Counter reset doesn't produce insane rates (if cumulative counters apply).

## 14. Verify

```bash
./addons/check.sh
```

## Available flag bits

| Bit | Flag | Purpose |
|-----|------|---------|
| 0 | `flagHasMin` | Min block present |
| 1 | `flagHasMax` | Max block present |
| 2 | `flagHasData` | Data block present |
| 3 | `flagHasApps` | Application section present |
| 4 | `flagReducerV2` | Exhaustive reducer provenance |
| 8 | `flagHasApache2` | Apache2 block present |
| 9 | `flagHasMysql` | MySQL block present |
| 10 | `flagHasPSU` | Power-supply section present |
| 11 | `flagHasMeanStats` | Trailing contributing statistics |
| 12 | `flagHasDiskIDs` | Persistent disk identities |
| 13 | — | **Next available** |
| 5–7, 14–15 | — | Available |

Use bit 13 next. **Never reuse a bit.**

Next: [Packaging & Release](15-packaging.md).

Contributing mean statistics use preamble bit 11 and a record-level trailer after all
Data/Min/Max blocks (`aggregation_codec.go`). Preserve that trailer when extending metrics;
new metric sections use bit 13 next and still append within each variable block.
