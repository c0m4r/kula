# Kula 0.20.0 — Pre-release Code & Security Review

Full-surface audit of everything changed since the `0.19.0` tag, plus an explicit
ship-readiness check: the release gate, the fuzz targets, the browser regressions, the
project's own black-box scanner against a running 0.20.0 build, and an in-place upgrade
test from real 0.19.0 tier files.

| | |
|---|---|
| **Commit** | `3691f28` (branch `main`, clean tree) |
| **Version** | `VERSION` = `0.20.0`; newest CHANGELOG section `[0.20.0] - TBA` |
| **Diff since 0.19.0** | 181 files, +29,476 / −4,101 (14,278 / 2,327 excluding JS) |
| **`./addons/check.sh`** | **pass** — govulncheck clean, `gofmt -l` clean, `go vet` clean, `go test -v -race ./...` all packages ok, golangci-lint clean |
| **Fuzzing** | 13/13 `Fuzz*` targets survived 15 s each (incl. `FuzzDecodeSample`) |
| **Browser regressions** | `./addons/test-frontend-regressions.sh` **pass** — 20-chart and 46-chart/165,600-point fixtures, `"errors":[]` |
| **`kula-scan` (black box)** | **44 pass / 0 fail** incl. `-aggressive -fuzz` against a live 0.20.0 instance |
| **Findings** | 3 medium, 19 low, **0 high** (+ 10 pre-existing issues carried over) |

Severity reflects impact on a default deployment. Where a finding is not self-evident from the
code, it carries a confidence rating: `high` means I read the exact code path *and* the data
that reaches it, `medium` means the mechanism is certain but how often it fires in the field is
not. The two headline frontend findings (M-1, M-2) were confirmed on the code path — the
triggering record already exists in the repo's own browser fixture. Findings confirmed by
**running** something (aggregation probes, the 0.19.0→0.20.0 upgrade, the live scans) are
described under [How this was verified](#how-this-was-verified).

**Verdict: the code is ready to ship.** No high-severity defect, no correctness or security
regression, and every headline claim of the release held up under independent testing. Some
items need a decision before the tag is cut — two small frontend fixes (M-1, M-2) and two
release-process items (M-3). None of them is architectural, and none affects stored data.

---

## Contents

- [Medium](#medium)
- [Low — new code](#low--new-code)
- [Low — release hygiene and docs](#low--release-hygiene-and-docs)
- [Low — pre-existing, carried over from the 0.19.0 review](#low--pre-existing-carried-over-from-the-0190-review)
- [What holds up](#what-holds-up)
- [How this was verified](#how-this-was-verified)
- [Suggested order](#suggested-order)

---

## Medium

### M-1 · MEDIUM · CONFIRMED — System Info grades every sensor with temperature thresholds, so every fan is painted "critical"

**`internal/web/static/js/app/system-info.js:733` and `:930`**

```js
const item = node('li', `system-info-inventory-item ${temperatureSeverity(sensor.value)}`.trim());  // 733
const severity = temperatureSeverity(sensor?.value);                                                 // 930
```

`temperatureSeverity` (`:707`) is a bare numeric threshold — `>= 90` →
`is-critical`, `>= 75` → `is-hot`, `>= 60` → `is-warm` — and neither call site checks the
sensor kind. The server puts **fans, voltages and currents in the same `sensors` array as
temperatures**, each with its own unit and kind:

```go
// internal/sysinfo/hardware.go:122
{"temp", "°C", SensorTemperature, 1000}, {"fan", "RPM", SensorFan, 1}, {"in", "V", SensorVoltage, 1000},
```

So a fan spinning at 1240 RPM is `>= 90` and gets `is-critical`, which
`internal/web/static/style.css:1019` and `:1024` render as a red value **and a red icon**.
Humidity ≥ 60 % is `is-warm` for the same reason.

The System Info page is new in this release, so this is new-code behaviour. The repo's own
fixture already contains the triggering record — `internal/web/testdata/system_info_client.js:35`
has `{device:'board', name:'CPU fan', value:1240, unit:'RPM', kind:'fan'}` — but the fixture
never asserts that fan's CSS class, which is why the browser gate stays green.

**Trigger:** open System Info → Sensors on any host exposing an hwmon fan.

**Fix:** gate both call sites on the kind the payload already carries
(`sensorKind(sensor) === 'temperature'`), the same helper the section already uses for icons
and labels.

---

### M-2 · MEDIUM · CONFIRMED — System Info Overview never refreshes uptime while the page is open

**`internal/web/static/js/app/system-info.js:285`, `:939-952`, `:920-933`**

```js
[label('kernel'), system.kernel],
[label('uptime'), data.live?.sys?.uptime_human],   // → <strong> at :288, dataset.copy = String(value)
```

The 5-second poll (`:1204`) calls `structureKey` to decide whether to re-render. That key
deliberately excludes measured values so a steady machine does not rebuild the DOM — but from
`live` it includes **only** `gpu` (`:950`), and `patchValues` only rewrites nodes carrying
`[data-live]` (`:920-933`). The uptime `<strong>` has neither, so it keeps the value from the
first render until a structure change, tab switch or language change. The click-to-copy chip
(`strong.dataset.copy`) copies the same stale string, while the "Copy summary" path
(`summaryText`) uses fresh data — so the two disagree.

Server uptime is minute-granular, so the displayed value visibly drifts within minutes on the
page whose whole purpose is "at-a-glance resource summaries".

**Fix:** give the uptime fact a `data-live` hook (and a `liveContent` branch), or include
`live.sys` in the patch path.

---

### M-3 · MEDIUM · RELEASE DECISION — 64 new UI strings ship English-only in all 25 non-English locales

`en.json` gained **210 keys** in this cycle (121 `si_*`, 89 other). All 25 other locales
received the 121 `si_*` keys and **none of the 64 non-`si_` keys** that the new surfaces use:

`historical_navigation, history_back, history_forward, zoom_out, live, pinned_time,
pinned_time_cleared, tier, coverage, contributors, source_records, bucket_start, bucket_end,
bucket_minimum, bucket_maximum, minimum, maximum, representative, policy_center, range_band,
complete_range, partial_range, range_outside_retention, range_whole_seconds, range_max_31_days,
range_dates_required, range_start_before_end, download_csv, download_chart_csv,
download_csv_all_data, data_table, chart_data_table, view_chart_data, chart_keyboard_help,
interactive_time_series, select_series, series, more_series, no_chart_data, chart_canvas_fallback,
calendar_month, calendar_year, previous_month, next_month, pick_start_day, pick_end_day,
showing_latest, updated, timestamp, timestamps, value, of, up_to, output_resolution, observations_per_series, …`

Every one of them is referenced by shipped UI code, and none exists in any locale file. At
`0.19.0` **every locale carried 100 % of `en.json`** (`git show 0.19.0:internal/i18n/locales/…`),
so this release drops non-English coverage from 100 % to 83 % of keys, on exactly the new
feature surface this release is about (history navigation, coverage notices, CSV/Data controls,
date picker, chart accessibility).

This is deliberate and documented — `docs/dev/11-i18n.md:58-59` says the new history strings
"currently use this English fallback … an explicit localization limitation" — and the fallback
is graceful and tested. But `TestCurrentUITranslationsCoverEveryLocale` only enforces `si_*`
plus a fixed list, so **nothing in the gate can detect this class of gap**, and re-checking
coverage after the fact will show only that the test passes.

**Decision needed:** translate the 64 keys before the tag, or accept the documented limitation
explicitly in the CHANGELOG entry for the new dashboard features.

---

## Low — new code

| # | Finding | Evidence | Confidence |
|---|---|---|---|
| L-1 | Unknown PCI base class is written as an empty string, contradicting the package contract "missing attributes are omitted" | `internal/sysinfo/hardware.go:77-80` does `d["class"] = pciClasses[(class>>16)&0xff]` unconditionally; `pciClasses` (`ids.go:156-178`) has no entry for `0x14`–`0x3f` or `0xff`, so the map read yields `""`. The UI falls back (`system-info.js:592` `pci.class \|\| 'other'`), so the impact is a stray `"class":""`. | high |
| L-2 | A drive swap that reuses a persistent ID also reuses the removed drive's counter baseline **when `diskseq` is unavailable** | `internal/collector/disk_identity.go:87` stores `diskseq`; `internal/collector/disk.go:251` requires `prev.diskseq == cur.diskseq`. Duplicate-ID detection only sees devices present in the same `/proc/diskstats` snapshot (`disk_identity.go:96`), and `diskseq` is Linux 5.14+, so on 5.10/5.4/4.19 LTS both sides are `""` and the guard passes. A probe (one drive `serial:\|\|A` replaced by a different drive with the same serial) produced `ReadsPerSec:4.9999e+06` where the `diskseq`-present path correctly yields 0. One bogus sample plus a permanently merged series for two physical drives — much narrower than 0.19.0's name-only tracking, which had no protection at all, but the CHANGELOG lists this failure class as "Fixed". | high on mechanism, medium on frequency |
| L-3 | `internal/sysinfo`'s refresh gate is stamped *before* discovery, so a pass longer than 5 s defeats the cache | `sysinfo.go:33-36` compares against `s.Timestamp`, set at the start of the pass; `p.cached = s` only at `:87`. If one pass exceeds `RefreshInterval` (5 s) the published snapshot is already stale, so every request re-runs full discovery (serialized on `p.mu`, but one pass per request). Needs a pathologically slow sysfs. | high on logic, narrow trigger |
| L-4 | `downsampleHistory` is dead production code | `internal/storage/query_planner.go:227` is called only from tests (`store_test.go:994,1035,1051,1075`, `source_budget_test.go:194`). Production goes `readHistory → reduceHistoryBuckets` directly (`history_stream.go:19`). The underlying functions are shared, so coverage is not lost — but five tests assert against a wrapper the shipped path never executes, and a future edit to one without the other is invisible. Delete it and point those tests at `readHistory`. | high |
| L-5 | Live sensor severity is patched by name only, ignoring the device | `system-info.js:926` finds by `item.name === name`, while the render path (`:907`) and the box key (`:741`) use device + name. Two devices exposing the same name (two NVMe "Composite", unlabelled `temp1`/`fan1`) colour the wrong row on every poll. | high |
| L-6 | `'refreshing'` has no translation key in any locale | `system-info.js:1211` `label(snapshot ? 'refreshing' : 'loading')`; neither `si_refreshing` nor `refreshing` exists in `en.json` or any locale, so `humanize()` produces English "Refreshing" for every language. Same class as M-3, one key. | high |
| L-7 | The RAM card can render the storage-only empty text | `system-info.js:897` calls `usageMeter(null)` before the first collector sample, and `usageMeter` (`:854-860`) falls back to `label('no_storage_usage')` = "Capacity is not measured for this mount". Reachable on a cold start, which the page otherwise handles (`si_metrics_pending`). | high on path |
| L-8 | Clipboard fallback can retain its scratch textarea | `system-info.js:1064-1074` removes the hidden `<textarea>` after `execCommand`, but the `catch` returns without a `finally`, so the failure path — the only reason this legacy branch exists — leaks one node per failed copy. | medium-high |
| L-9 | PCI bridges are grouped under "Other" although the server emits a `bridge` class | `system-info.js:557-565` `classOrder`/`classIcons` have no `bridge`; `internal/sysinfo/ids.go:163` emits `0x06: "bridge"` and every locale ships `si_class_bridge`. Cosmetic; may be intentional. | high |
| L-10 | Tooltip range state uses `exact_complete` where the status area uses `complete` | `history-data.js:94-96` feeds `format.js:205-212`, so a preset window whose newest sample is a few seconds old renders `coverage: 100% · partial range` in the tooltip while the status line reports complete. The Go contract (`storage/store.go:344-348`) reserves `exact_complete` for exact/custom views, and `charts-data.js:1241-1248` follows that rule — the tooltip does not. | medium |

## Low — release hygiene and docs

| # | Finding | Evidence |
|---|---|---|
| L-11 | Two CHANGELOG "Changed" entries hide user-visible behaviour | `CHANGELOG.md:34` "Tier naming unification" is the 1-based → 0-based tier display: `0.19.0` rendered `` `Tier ${tier + 1}` `` (`charts-data.js` 0.19.0:1611) and the dashboard now renders `` `${i18n.t('tier')} ${tier}` `` (`format.js:195`, `charts-data.js:1913,1942`), so raw data reads "Tier 0" where it read "Tier 1". `CHANGELOG.md:35` "Inspect feature improvements" ships a new flag (`kula inspect --verbose`, `cmd/kula/main.go:52-53,63-70`) that the Added section never mentions. Neither is wrong, but neither is discoverable. |
| L-12 | The Ansible template still teaches kernel-name disk selection | `addons/ansible/roles/kula/templates/config.yaml.j2:36` `# devices: ["sda", "nvme0n1"]`, while `config.example.yaml:41-48` now shows `wwid:` IDs and warns that legacy names follow whatever drive currently holds the name (`docs/user/11-prometheus.md:131-133`). Operators copying the shipped template keep the fragile selector this release exists to replace. |
| L-13 | Prometheus docs enumerate 2 of 3 `identity_source` values | `docs/user/11-prometheus.md:127-128` documents `wwid` and `kernel`, but `internal/collector/disk_identity.go:187` also builds `serial:<vendor>\|<model>\|<serial>[:ns:N]`, and `IdentitySource()` returns `serial`. Alert rules written from the docs silently miss serial-identified drives. |
| L-14 | Stale record-size figure in the testing guide | `docs/dev/12-testing.md:173` says **8,190 bytes**; the test prints `rich aggregate: 8199 bytes` (`codec_test.go:798`). The +9 B is exactly the new disk-ID trailer: 3 variable blocks × (version u8 + count u16). |
| L-15 | "all four checks" — the gate runs five | `docs/dev/14-adding-metrics.md:10-11` omits `gofmt -l .` (`addons/check.sh:24-25`), which the other two contributor docs list correctly. |
| L-16 | The browser gate can hard-fail where it should skip | `addons/test-frontend-regressions.sh:12` accepts Chromium anywhere on `PATH` (or `$KULA_CHROMIUM`), but `internal/web/testdata/chromium_test_helper.mjs:8-13` only checks `$KULA_CHROMIUM` plus four hard-coded `/usr/bin/...` paths. With a snap-installed browser the fixtures fail with "Chromium/Chrome not found" instead of the documented skip. |
| L-17 | `web.graphs.gpu_temp` is accepted but silently ignored, and is absent from the example and docs (pre-existing) | `internal/config/config.go:200` defines it and the frontend asks for it (`charts-init.js:430`, `split.js:840`), but `/api/config` emits only `cpu_temp`, `disk_temp`, `network`, `split` (`internal/web/server.go:769-790`), and `config.example.yaml:212-221` has no `gpu_temp:` entry. An operator setting `max_mode: on` gets no bound and no error. |
| L-18 | AUR release step is incomplete | `docs/dev/15-packaging.md:22-24` mentions only re-uploading `CHECKSUMS.sha256.txt`, but `addons/install_v2.sh:299` downloads `kula-<ver>-aur.tar.gz` (built at `build_aur.sh:288`, checksummed `:294`), and `build_aur.sh:24,293` also lists the GitHub source tarball, so a blanket `sha256sum -c` over release assets reports a missing file. |
| L-19 | CI/packaging gardening | `.github/workflows/ci.yml:28-38` path filter omits `.golangci.yml`, which both CI and `check.sh` consume; `ci.yml:80` "(1.26.1)" and `snap/snapcraft.yaml:121,127` "1.26.4" are stale against `go.mod:3` `go 1.26.8` (the workflow itself reads `go-version-file`, so nothing breaks). |

## Low — pre-existing, carried over from the 0.19.0 review

Re-verified still open in this tree; none is a 0.20.0 regression, listed so the release notes
and the next cycle have them in one place.

| # | Finding | Evidence |
|---|---|---|
| C-1 | `nvidia.log` is read unbounded once per collection tick (0.19.0 K-07) | `internal/collector/gpu_nvidia.go:16,44` `io.ReadAll(f)` with no `io.LimitReader`, unlike every HTTP read in the collector. |
| C-2 | Upstream error text is written into an SSE frame unescaped (K-08) | `internal/web/ollama.go:361` `fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())`. |
| C-3 | Model name interpolated into `innerHTML` (K-09) | `internal/web/static/js/app/ollama.js:49` `` select.innerHTML = `<option value="${ollamaModel}">${ollamaModel}</option>` ``. |
| C-4 | nginx/apache2 status URLs get no scheme or host validation (K-12) | `internal/collector/nginx.go:33`, `apache2.go:41` pass the configured URL straight to `Get`. |
| C-5 | Custom-metrics socket is removed blind (K-13) | `internal/collector/custom.go:107` `os.Remove(sockPath)` without checking whether a live instance owns it. |
| C-6 | CSP keeps `style-src 'unsafe-inline'`; the nonce ignores the `crypto/rand` error (K-17, K-18) | `internal/web/server.go:244`, `:232` `_, _ = rand.Read(b)`. |
| C-7 | `parseUintBytes` has no overflow guard | `internal/collector/util.go:23-28` `n = n*10 + uint64(ch-'0')`; a >u64 field wraps silently. Not reachable from a real kernel — only from a bind-mounted crafted `/proc` — and 0.20.0 adds call sites in `disk.go:131-134`. |
| C-8 | `used` can underflow on an inconsistent `statfs` | `internal/collector/disk.go:441` `used := total - (stat.Bfree * uint64(stat.Bsize))` with no `Bfree > Blocks` guard; a broken FUSE/network mount yields ~2^64 used and stores it. |
| C-9 | `join_charts` is missing from every locale, so the ⊞ tooltip reads the raw key | `split.js:497` `i18n.t('join_charts') \|\| 'Join charts'` — `i18n.t` returns the truthy key, so the fallback is dead. Identical in 0.19.0. |
| C-10 | An untracked 50 ms resize timer can fire at a destroyed chart | `split.js:580` `setTimeout(() => chart.resize(), 50)`; teardown destroys the instances (`:462-471`) and vendored `destroy()` nulls `canvas`, so a ⊞/⊟ within the window logs a `TypeError`. |

---

## What holds up

Everything below was checked specifically because it is either a hard project invariant or a
release claim that would be expensive to get wrong.

**The codec and its mirror.** Preamble bits match the documented map exactly (used 0–4, 8–12;
free 5–7, 13–15; bit 13 next), the disk-ID section is appended after PSU, and the record-level
mean-statistics trailer still follows all Data/Min/Max blocks. `addons/inspect_tier.py` mirrors
the flags, the section order, the `hasDiskIDs ⇒ hasApps && hasPSU` gate, the disk-ID entry
encoding and the mean-stats trailer validator — 188 byte-consuming reads align 1:1 with the Go
decoder, and the cross-language tests in `disk_identity_test.go` / `aggregation_codec_test.go`
run here (python3 present).

**Reading 0.19.0 data with the 0.20.0 binary (VERIFIED).** I built the generator from the actual
`0.19.0` tag, wrote a day of legacy tier files with it, and pointed the new binary at them:
tier 0 raw records decode, legacy tier-1 buckets come back with
`extrema_profiles: {legacy: [12 audited fields]}`, `valid_aggregations: ['data']` and
`available_aggregations: ['data','min','max']` — the strict old contract preserved, the union
added. Across 289 buckets I checked 3,468 `min ≤ data ≤ max` triples on the restored fields:
**zero violations**. Disk series split exactly as documented: legacy records under
`kernel:<name>`, new records under `wwid:…`, with the downgrade caveat already stated in
`README.md:354-356`.

**Aggregation arithmetic (VERIFIED).** Four probes through the public API:
60 one-second samples with `cpu.total.usage = 0…59` roll up to one tier-1 bucket with mean
**exactly 29.5** (duration-weighted), min 0, max 59, memory extrema 1000…1059,
`AggregationVersion ≥ 2`, profile `current`. A 30-second outage splits the window into two
records whose extrema stay inside their own contiguous run — no smearing across the gap.
Extrema are genuinely per-series (cpu min 0 coexists with load max 1000 in one bucket, which a
snapshot-based reducer could not produce). Dynamic collections keep their `identity` through the
rollup: one series per disk ID, per-device means and extrema.

**The rewritten storage query path.** Source-tier selection is bounded by a fixed record budget
independent of the requested point count; the snapshot-expiry retry cannot publish partial data;
the query cache is keyed on exact bounds, invalidated on write by generation and live-edge
lookback, capped at 256 entries, and cloned on the way out so no caller can mutate a cached
result; lock ordering (`s.mu` → `queryCacheMu`) is consistent between the writer and the reader.
Live checks confirm the behaviour end to end: 1 h → tier 0 at 2 m, 6 h → tier 1 at 1 m
(tier 0 correctly skipped at 21,600 source records), 24 h → tier 1 downsampled to 5 m, 30 d and
exactly 31 d accepted, 31 d + 1 s rejected with a clear 400, and the `sections=` parameter
shrinking a Focus Mode payload by ~8× while returning `sections`/`available_aggregations`
consistently.

**Security posture of the changed web layer.** The WebSocket race is genuinely fixed by moving
`sendCh`'s lifetime into the hub under the mutex that also removes the client, plus the
`client.closed` guard for the register/unregister ordering — no path can send on a closed
channel now. Session lifetime is enforced in `ValidateSession`, `CleanupSessions`,
`LoadSessions` and the WebSocket re-check, and the sliding expiry is clamped so a persisted
`expiresAt` never overstates the session's life. The login limiter is keyed on IP+username with
the IP first, so the attacker-controlled half cannot forge another caller's prefix, the IP
counter (15) stays above the per-account counter (5) so the per-account limiter is reachable,
and the per-user map fails open only under deliberate memory pressure while the IP limiter still
bounds the caller. `kula-scan` against the running build: **44 pass / 0 fail** including
`RATE-LOGIN` (429 within a burst), `BYPASS-XFF` (spoofed `X-Forwarded-For` does not bypass the
throttle), `DOS-SLOWLORIS`, `DOS-CONNFLOD`, `CSRF-001..003`, traversal, header injection and
`FUZZ-*` — with zero panics in the server log afterwards and the process still serving.

**Chart lifecycle invariants.** `_updateHiddenIndices()` exists in the vendored Chart.js and is
called correctly (method call, right `this`, `?.`-guarded); `_getUniformDataChanges` really does
drain `_dataChanges`, so culled charts cannot accumulate mutation records, and a culled-but-dirty
chart keeps its `dirty` entry and re-arms a flush when it becomes visible. `dirty.delete` happens
before `chart.update()`, `render` never downgrades an `update`, and `split.js` contains **zero**
`document`/`window` listeners — the per-card listener that leaked discarded cards in 0.19.0 was
deleted. Outside-click dismissal still works for bubbling icon clicks while menus no longer
self-dismiss on open.

**The new System Info surface.** `/api/system-info` is registered inside the `/api/` subtree
wrapped by `AuthMiddleware` (401 without a session — confirmed over the wire), returns 404 when
`global.show_system_info` is false, ignores all request input so it is not a file-read or SSRF
primitive, bounds every read with `io.LimitReader(f, 2<<20)`, and caches discovery for 5 s so a
request cannot amplify sysfs work. The CodeQL "unvalidated dynamic method call" finding is
genuinely fixed — the renderers dispatch through a `switch` and a `Set` membership test, and
`system-info.js` has no HTML sink at all: every string, including USB/DMI/device names, goes
through `textContent`.

**Frontend safety properties.** `HistoryRequestController` supersedes and aborts on every new
request and re-checks `isCurrent` before *and* after applying, so a slow response cannot
overwrite a newer range; `clampHistoryInterval` cannot invert or produce a zero/negative span;
`minimumZoomSpan` parses every string `fmtRes` can emit and degrades safely; the 12-point zoom
floor, gap shading, Data/CSV opt-in and accessibility alternatives are all asserted by the
browser fixtures. The four deleted modules (`ui-actions.js`, `url-state.js`, `theme.js`,
`utils.js`) have no dangling references anywhere in the repo, and all 31 modules resolve their
imports.

---

## How this was verified

- **Static gate**: `./addons/check.sh` — govulncheck (no vulnerabilities reachable), `gofmt -l`
  clean, `go vet ./...` clean, `go test -v -race ./...` all packages ok, golangci-lint clean.
  Re-run with `-count=1` for `internal/config` to defeat the test cache.
- **Fuzzing**: `./addons/fuzz.sh 15s` — all 13 `Fuzz*` targets survived; the storage/codec
  targets are the ones that matter for the new sections.
- **Browser regressions**: `./addons/test-frontend-regressions.sh` — both fixtures green,
  covering mutation retention, visibility after trimming, split-card cleanup, session-expiry
  re-login, legacy extrema compatibility, stable disk identity, gesture isolation and the
  12-point zoom floor.
- **Black box**: `kula-scan` (built from this tree) against a live 0.20.0 instance, first in the
  default mode (30 pass / 0 fail / 2 warn), then `-aggressive -fuzz -fuzz-iter 200`
  (44 pass / 0 fail / 3 skip). The two warnings are configuration, not code: plaintext HTTP and
  `/metrics` without a token (both deliberate in the test config). The skips are TLS (no TLS
  listener), Ollama (not enabled) and one enumeration probe deferred because the rate limiter
  had correctly engaged.
- **End-to-end**: built the binary, generated 8 h of fixture data, served it with auth enabled,
  and exercised `/api/current`, `/api/config`, `/api/history` across point budgets, ranges,
  `sections=` filtering and the error paths, the UI, `/metrics` (106 sample lines, all
  parseable, none in exponent notation) and `/api/system-info`; then restarted the server on the
  same tier files to exercise reopen/rollup reconstruction. No panics, no errors, clean shutdown.
- **Upgrade path**: generated real 0.19.0 tier files with a binary built from the `0.19.0` tag,
  opened them with the 0.20.0 binary, and validated metadata and numeric consistency as
  described above.
- **Independent probes**: four temporary tests in `internal/storage` for the aggregation claims
  (removed afterwards), plus overlay-mounted probes for the disk-ID, PCI-class and sysinfo-cache
  mechanisms. The workspace is byte-identical to how I found it: `git status` is clean, `go.mod`
  /`go.sum` were restored after a `-mod=mod` run touched them, and the temporary tree is deleted.
  The only file added is this review.

**Environment notes for the maintainer** (not repo defects): in this sandbox `$HOME` is
read-only, so `go test ./internal/config/` fails with "insufficient permissions to create data
storage" until `HOME` is redirected to a writable directory; govulncheck and the Go build cache
need `GOCACHE` pointed somewhere writable for the same reason. With that redirection every stage
of the gate passes. Nothing else in the suite depends on the environment.

**Scope note**: this review covers everything changed since `0.19.0`. Pre-existing issues are
listed only where they interact with the changed code (C-1…C-10) and are attributed as such;
they were not re-audited in depth.

---

## Suggested order

1. **M-1** — one-line kind check in `sensorItem`; the payload already carries the kind.
2. **M-2** — give the uptime fact a live hook so the new System Info page stops showing a frozen
   number.
3. **M-3** — decide translate-or-document for the 64 keys; if documenting, say so in the
   CHANGELOG entries for the new dashboard features.
4. **L-5, L-6, L-8, L-10** — small, self-contained; L-10 is worth a moment's thought about which
   flag the tooltip should present.
5. **L-1, L-2, L-3, L-4** — cheap correctness/hygiene in the new Go code; L-2 deserves at least a
   line in the disk-identity docs, since it is the one remaining path where two physical drives
   can share a series.
6. **L-11 … L-19 and C-1 … C-10** — backlog; none blocks the tag.
