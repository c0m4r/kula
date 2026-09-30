# Kula 0.21.0 — Pre-release Code & Security Review

Full-surface audit of everything changed since the `0.20.3` tag plus an explicit ship-readiness
check: the release gate, every fuzz target, the browser regressions, the project's own black-box
scanner against a live 0.21.0 build, and first-hand verification of the four riskiest new
subsystems — the read-only storage reader behind the TUI History view, the startup CSS/JS
minifier, the TV-mode/PNG-export frontend, and the new CI/packaging tooling.

| | |
|---|---|
| **Commit** | `f412f62` (branch `main`, clean tree; `git describe` = `0.20.3-46-gf412f62`) |
| **Version** | `VERSION` = `0.21.0`; newest CHANGELOG section is `[Unreleased]` |
| **Diff since 0.20.3** | 129 files, +14,252 / −1,000 (Go: 20 files, +4,922 / −205) |
| **`./addons/check.sh`** | **pass** — `gofmt -l` clean, `go vet ./...` clean, `go test -v -race ./...` all 12 packages ok, golangci-lint `0 issues` |
| **govulncheck** | **no vulnerabilities found** (0 called; 1 module-level advisory in a required module, not called) |
| **Fuzzing** | **15/15** `Fuzz*` targets survived 20 s each (2 new: `FuzzMinifyJS` ≈629 k execs, `FuzzMinifyCSS` ≈574 k execs) |
| **Browser regressions** | `./addons/test-frontend-regressions.sh` **pass** — both fixtures, `"errors":[]`, incl. `chart_image_export`, `custom_picker`, `calendar_range` |
| **`kula-scan` (black box)** | default **32 pass / 0 fail / 1 warn**; `-aggressive -fuzz` **47 pass / 0 fail / 1 warn / 2 skip** (one better than 0.20.0's 44/0) |
| **Findings** | 3 medium in new code, 2 medium pre-existing but newly exercised, 10 low in code/tooling, 3 release-hygiene notes |
| **Status at `2a7f63e`** | **all 5 medium fixed**; 11 of 13 low fixed, L-1 partly fixed (JS half open), L-11 open (CHANGELOG, maintainer), L-8 accepted |

Severity reflects impact on a default deployment. Findings are marked **new** (introduced or
materially changed in this release) or **pre-existing** (older code that this release's features
now reach). Every finding was checked against the exact code path, and every runtime claim was
reproduced by running something — the minifier, npm, the scanner, a live server, or instrumented
probes in a copy of the tree ([How this was verified](#how-this-was-verified)). Two headline claims
were tested against real artifacts rather than the repo's own fixtures: the minifier was run over
the actual upstream Chart.js bundles through an independent parser, and the corrupt-tier allocation
was measured against a real ring file.

**Verdict: the code is ready to ship.** No high-severity defect, no security regression, and no
on-disk format change — the codec, tier layout and aggregation policies are untouched, so
existing history is read exactly as before. The two things worth fixing before the tag are both
small and self-contained: the JS minifier mis-lexes a regex literal in two keyword contexts and
silently drops whitespace *inside* it (M-1), and TV mode's grid math ignores the stylesheet's row
floor (M-3). M-2 (supply-chain script that can pass vacuously) is a release-process guard, not a
shipping defect. The remaining mediums are older code that this release's new workflows expose;
none blocks a tag.

> **Status at `2a7f63e`:** fixed in `d289524`…`2a7f63e`, one commit per area. Every medium is
> fixed. Still open: **L-11** (the CHANGELOG, which the maintainer writes) and the JS half of
> **L-1** (the script check still reuses the lexer it tests). **L-8** was accepted as documented.
> Each finding below names its fix commit. `Opus_5.5.md` Part 3 in this directory has the full
> resolution table.

---

## Contents

- [Medium](#medium)
- [Low — new code](#low--new-code)
- [Low — tooling, CI and packaging](#low--tooling-ci-and-packaging)
- [Low — release hygiene and docs](#low--release-hygiene-and-docs)
- [Previous review's findings](#previous-reviews-findings)
- [What holds up](#what-holds-up)
- [How this was verified](#how-this-was-verified)
- [Suggested order](#suggested-order)

---

## Medium

### M-1 · MEDIUM · CONFIRMED · FIXED — `new` — the JS minifier drops whitespace *inside* regex literals it mis-lexes as division

**`internal/web/minify.go:110-114` (`jsKeywordsBeforeExpression`), `:346-347` (if/for/while/with head detection), `:368-387` (`jsRegexAllowed`)**

> **Status: fixed in `d538ddc`.** `default` is in `jsKeywordsBeforeExpression`, `for await (` now
> opens a loop head, and a word after `.`/`?.` is never taken as a keyword. Both inputs are
> `TestMinifyJS` cases and `FuzzMinifyJS` seeds. The stronger fix, asserting that regex, string
> and template bytes survive verbatim, was not added (see L-1).

```go
var jsKeywordsBeforeExpression = map[string]bool{
    "return": true, "typeof": true, ... "extends": true,   // "default" is missing
}
...
case "(":
    head := prev != nil && prev.kind == jsWord &&
        (prev.text == "if" || prev.text == "for" || prev.text == "while" || prev.text == "with")
```

When the heuristic decides a `/` is division instead of the start of a regex literal, the whitespace
inside what is really a regex is treated as ordinary token spacing and deleted. Two contexts reach
that state today: `export default /…/` (`default` is absent from the keyword set) and
`for await (…) /…/` (the token before `(` is `await`, so it is not recognised as a head).

Reproduced against the shipped minifier (temporary probe test, removed afterwards):

```
IN  "export default /[a-z] /;\n"
OUT "export default/[a-z]/;"                        (err=nil)
IN  "for await (const x of y) /[a-z] / .test(x);\n"
OUT "for await(const x of y)/[a-z]/.test(x);"       (err=nil)
```

Node confirms the change is semantic, not cosmetic: the input regex requires a trailing space
(`test("a ") === true`, `test("a") === false`); the minified one does not (`test("a") === true`).
All three guards miss it — `sameJSTokens` (`minify.go:155-165`) re-lexes with the *same* heuristic,
`node --check` accepts the synthesised output, and `calculateSRIs`/`staticETags`
(`server.go:1114-1136`, `:1149-1172`) hash the corrupt bytes, so a browser would load them with a
valid integrity tag.

**Trigger:** no shipped 0.21.0 asset contains `export default` at all (grep over
`internal/web/static/`: zero hits) or `for await`, so this is latent for the release. It becomes
live for the next first-party module that uses either form, and for the supported distro path
where `addons/packaging/replace_chartjs.sh:117-119` installs the **unminified** upstream trio and
pushes ~460 KB of third-party code through this heuristic at every startup. I ran the minifier over
those three real bundles and compared every literal with acorn 8.18.0 (independent parser): all
three minify without error, `node --check` passes, and the literal streams are **identical** — so
today's distro path is safe, but the failure mode is one upstream release away.

**Fix:** add `"default": true`; treat `for await (` as a head; add the two cases (plus fuzz seeds)
to `TestMinifyJS`. Stronger: assert that the bytes of every `jsRegex`/`jsString`/`jsTemplate` token
survive verbatim, instead of trusting the same lexer twice.

---

### M-2 · MEDIUM · CONFIRMED · FIXED — `new` — `verify-lock.js` reports success and exits 0 when the lockfile has no `packages` map

**`addons/chartjs/verify-lock.js:22, 25, 30-32, 55`**

> **Status: fixed in `03ac3ac`.** The script rejects a lockfile below v2 or without a root
> `packages['']` entry. It requires every `dependencies`, `devDependencies`,
> `optionalDependencies` and `peerDependencies` entry to be an exact pin with a
> `node_modules/<name>` entry, and it fails on an empty lock. The real lockfile still passes
> (32 packages). The L-9 fix makes CI run on `addons/chartjs/**`.

```js
const root = lock.packages?.[''] ?? {};
for (const [name, spec] of Object.entries({ ...root.dependencies, ...root.devDependencies })) { ... }
const locked = Object.entries(lock.packages ?? {}).filter(([path]) => path)...
console.log(`${locked.length} locked packages pinned, from ${REGISTRY}, and at least ${minDays} days old`);
```

`addons/build-chartjs.sh:40` runs this script as the supply-chain gate before `npm ci`. A lockfile
without a `packages` map (`lockfileVersion: 1`, e.g. after a bad merge or an older-npm rewrite)
yields zero entries to check and still prints the affirmative success line. Reproduced
independently:

```
$ node addons/chartjs/verify-lock.js /tmp/l1.json 14     # lockfileVersion 1, no integrity anywhere
0 locked packages pinned, from https://registry.npmjs.org/, and at least 14 days old
exit=0
```

npm still installs from such a lock, so the documented guarantee in AGENTS.md — every locked package
exact-pinned, from the registry, sha512-integrity-checked and ≥14 days old — is vacuously satisfied.
Verified end to end: a synthetic `lockfileVersion: 1` lock with **zero** `integrity` fields passed
the guard (`0 locked packages pinned …`, exit 0) and `npm ci` then reported
`npm warn old lockfile` and `added 1 package`. The root pin
check also only inspects `dependencies` and `devDependencies`: a root `optionalDependencies:
{"evil": "^1.0.0"}` with no matching `node_modules/` entry also exits 0 (verified).

**Why it matters:** the affirmative success line is what a release gate trusts; the failure needs
write access to a reviewed file, so this is defence-in-depth rather than a live hole. It composes
with L-9 (no CI runs for `addons/chartjs/**`), which means a lockfile-only PR is both unchecked by
CI and able to pass the local guard.

**Fix:** require `lockfileVersion >= 2` and a non-empty `packages` map; assert that every root
`dependencies`, `devDependencies`, `optionalDependencies` and `peerDependencies` entry exists as
`node_modules/<name>`.

---

### M-3 · MEDIUM · CONFIRMED · FIXED — `new` — TV mode sizes the grid from a height the stylesheet will not honour, so charts run off the screen

**`internal/web/static/js/app/tv-mode.js:31-48` (`tvGridShape`), used at `:92`; `internal/web/static/style.css:3299-3306`**

> **Status: fixed in `7577d21`.** The floor is now a stylesheet variable, `--tv-row-min: 10rem`.
> `tvGridShape` reads it in pixels, so it follows the Text size setting, and only considers shapes
> whose rows fit. The example from this finding (16 charts, 1920×400) now yields 8 × 2, asserted
> in `history_frontend_test.mjs`.

```js
const cellHeight = (height - gap * (rows - 1)) / rows;   // no floor
const chartWidth = Math.min(cellWidth, cellHeight * aspect);
```
```css
html.tv-mode #charts-grid { overflow-y: auto;
    grid-template-rows: repeat(var(--tv-rows, 1), minmax(10rem, 1fr));
    grid-auto-rows: minmax(10rem, 1fr); }
```

The heuristic maximises chart width as if rows could shrink without bound; the browser clamps every
row to `10rem` (160 px). On a short, wide display — exactly the wall-display case TV mode exists
for — the computed shape can be impossible: with ~1920×400 px of grid space and 16 selected charts
the search picks 6 × 3 believing each row is ≈130 px, the browser makes them ≥160 px, and the grid
becomes ~480 px tall inside a 400 px box (`overflow-y: auto`, so part of the grid scrolls out of
view while TV mode has hidden all dashboard chrome). `tv-mode.js:80` states the contract that is
violated: "Size the combined Focus Mode grid so every shown card fits on screen."

**Fix:** read the floor (custom property or `getComputedStyle`) and use
`Math.max(floor, (height - gap * (rows - 1)) / rows)`, or reject shapes where
`rows * floor + gap * (rows - 1) > height`.

---

### M-4 · MEDIUM · CONFIRMED · FIXED — `pre-existing` — `kula tui` unlinks the running server's custom-metrics socket

**`internal/collector/custom.go:107` (`_ = os.Remove(sockPath)`), `:130`; reached from `cmd/kula/main.go:274-276`**

> **Status: fixed in `8778664`.** The TUI no longer starts the custom-metrics listener
> (`apps.Custom = nil` in `runTUI`). `newCustomCollector` also dials the socket first, and refuses
> to unlink one that another process still serves. Covered by `TestCustomCollectorKeepsLiveSocket`.

```go
// newCustomCollector ...
// Remove any stale socket file
_ = os.Remove(sockPath)
listener, err := net.Listen("unix", sockPath)
```

`runTUI` builds a collector with `cfg.Storage.Directory` and calls `StartApplications()`, so the
TUI's own custom-metrics listener unlinks whatever socket is at `<storage>/kula.sock` — including
one a running `kula serve` is listening on. The server's listener survives on the orphaned inode
while nothing writes to the path any more, and when the TUI exits (`custom.go:287` removes the
path) the socket is gone entirely: custom-metric ingestion is silently dead until the server
restarts. The code and the collector call were both present at `0.20.3` (`git show
0.20.3:cmd/kula/main.go` shows the same `collector.New(..., cfg.Storage.Directory)` +
`StartApplications()`), so this is not a regression — but 0.21.0 documents and advertises running
the TUI against a live server's storage (`docs/user/06-tui.md`, README "History view"), which is
precisely the workflow that triggers it.

**Fix:** give the TUI a distinct socket path, or probe the socket (connect/flock) before removing
it and refuse to unlink one a live listener owns.

---

### M-5 · MEDIUM · CONFIRMED · FIXED — `pre-existing` — a corrupt record length prefix drives an allocation up to the header-claimed tier size

**`internal/storage/tier.go:501-502` (segment extents), `:556-558`, `:590` (`data := make([]byte, dataLen)`)**

> **Status: fixed in `8beb057`.** A length prefix above 64 MiB (`maxRecordBytes`) is treated as
> corruption, and the writer refuses records that large. Scanned segment extents are clamped to
> the real file size with one `Stat` per scan. The on-disk format is unchanged. The bound is well
> above a measured worst case of 18 MiB for a rollup record at the codec's interface maximum.

```go
dataLen := binary.LittleEndian.Uint32(hdr[0:4])
if dataLen == 0 || int64(dataLen) > t.maxData || bytesRead+4+int64(dataLen) > seg.size { ... }
...
data := make([]byte, dataLen)
```

Both bounds come from the file's own header (`maxData`, `writeOff`), not from the file's real size,
and the allocation happens before the read — so one flipped or torn length prefix in an otherwise
valid tier allocates up to `maxData` (default tier 0: 250 MB) and, with a corrupt header, up to the
uint32 ceiling (~4 GiB) before `io.ReadFull` fails on EOF. Measured on a copy of the tree: a full
64 MiB ring with one prefix claiming 60 MiB allocated 61.0 MiB; a 96-byte file whose header claims a
1 TiB ring allocated 257 MiB. The codec fuzz target only covers record payload bytes, so tier-level
framing is outside its coverage. Pre-existing (`tier.go` is unchanged in this release), but the new
read-only reader makes tier scanning part of the TUI's normal operation.

**Fix:** clamp the scanned extent and `dataLen` to the real file size (one `Stat` per refresh),
and/or cap a single record at a sane maximum — records are kilobytes.

---

## Low — new code

### L-1 · LOW · CONFIRMED · PARTLY FIXED — `new` — `TestEmbeddedAssetsMinify` cannot see whitespace-significant CSS corruption, and its JS half re-uses the lexer it is checking

> **Status: CSS half fixed in `d538ddc`; JS half open.** `TestEmbeddedAssetsMinify` now checks
> stylesheets with `cssGapsPreserved`, which shares no code with the minifier and rejects the
> dropped-space mutations described here. The regex-after-keyword inputs are `FuzzMinifyJS` seeds.
> The script check is unchanged: `TestEmbeddedAssetsMinify` and `FuzzMinifyJS` still compare
> `lexJS(src)` with `lexJS(min)`, so a new heuristic slip like M-1 would pass both again.

**`internal/web/minify_test.go:180-189`, `:202-228`, `:311-319`**

```go
strip := func(b []byte) string {
    s := cssSpace.ReplaceAllString(cssComment.ReplaceAllString(string(b), ""), "")
    return strings.ReplaceAll(s, ";}", "}")
}
if strip(src) != strip(min) { t.Errorf(...) }
```

Both sides are whitespace-stripped before comparison, so exactly the corruption class the minifier
can produce (M-1) is invisible; the JS branch compares `lexJS(src)` with `lexJS(min)`, i.e. the same
heuristic lexer twice. Demonstrated by the minifier reviewer: after making `cssNoSpaceAfter` also
drop the space after letters (producing `a.b>.c{}` from `a .b > .c { }` and
`@mediascreenand(max-width:600px)`), `TestEmbeddedAssetsMinify`, `FuzzMinifyCSS` and
`TestMinifiedScriptsParseInNode` all still pass. The other half of the invariant does hold — a no-op
CSS minifier fails the test with `static/style.css is served unminified` (verified).

**Fix:** compare against an independent CSS tokeniser, or assert every dropped-whitespace site has a
delimiter byte on at least one side; add fuzz seeds for the regex-after-keyword and ASI classes.

### L-2 · LOW · CONFIRMED · FIXED — `new` (root cause pre-existing) — DST-gap times are shifted an hour while the picker keeps showing the typed text

> **Status: fixed in `38b09fc`.** `existingDateTimeInput` returns the wall-clock time that a
> value actually resolves to. The picker shows that time, or shows it on blur if the field is
> being typed in, so the field no longer disagrees with the range that loads. A DST-gap case
> is in `history_frontend_test.mjs`.

**`internal/web/static/js/app/controls.js:354` (`endOfDay()`), `:357-366` (`customPresetRange`), `:370-371`; `internal/web/static/js/app/format.js:157-167`**

`parseDateTimeInput` builds `new Date(y, m, d, h, mi, s, ms)`. Where a zone skips a local hour
(e.g. America/Santiago 00:00→01:00, or the common 02:00→03:00), that constructor resolves the
non-existent local time with the pre-transition offset. Verified in node:

```
TZ=America/Santiago  new Date(2024, 8, 8, 0, 30)  ->  Sun Sep 08 2024 01:30:00 GMT-0300
reformatted local text: 2024-09-08T01:30
```

The field itself renders the canonical `dataset.time` text, so the user sees `00:30` while the chart
loads `01:30`, with no error. The parse predates the release (0.20.3 read `datetime-local` the same
way), but the new clock dial, wheels and Today/Yesterday presets reach it far more often.
**Fix:** after building the instant, re-format it and either rewrite the field or raise
`range_time_invalid` when it no longer matches the requested wall-clock time.

### L-3 · LOW · CONFIRMED · FIXED — `new` — a wrap during a cross-process scan can return records out of order, and the TUI plots them as-is

> **Status: fixed in `8beb057` (storage) and `a93a829` (plot).** A read-only scan re-reads the
> owner's header as it goes and retries once the owner has overwritten bytes it read, so torn
> records are no longer returned. The TUI's `inTimeOrder` also drops any bucket that does not
> follow the previous one. With more samples, `Opus_5.5.md` (Part 1, L-2) also measured wrong
> values, not only reordered ones. The fix covers both, and the storage docs now describe it.

**`internal/storage/tier.go:538-541`, `internal/storage/readonly.go:142-144`, `internal/tui/history.go:696-706`**

`writeCycle` only advances in `Refresh`, so a ring lap that happens *during* a read-only scan is
invisible to the reader. Measured (256 KiB single-tier ring, owner writing at full speed for 8 s,
47,759 writes): 35 of 50,472 returned raw samples (0.07 %) were out of timestamp order, by up to
~2 minutes. `historyData.layout()` assumes ascending timestamps, so a backwards point is drawn to
the left of its predecessor and the gap test misses it. `docs/dev/05-storage-engine.md:207-210`
promises only "end that segment early or skip a torn record", so this is a documentation-and-plot
mismatch rather than a data-loss bug. **Fix:** drop or sort non-monotonic points in `layout()`.

### L-4 · LOW · CONFIRMED (mechanism) · FIXED — `new` — one failed `OpenReadOnly` disables History for the whole TUI session

> **Status: fixed in `a93a829`.** `Options.OpenHistory` replaces the pre-opened store. The TUI
> retries the open every 5 s while the History view is shown, and closes the store on exit.

**`cmd/kula/main.go:289-292`, `internal/tui/history.go:143-145`, `:201-204`**

The open error is stored in `opts.HistoryErr` and never retried; with `source == nil` the view
returns early and `historyStale` is always false, so no load is ever attempted again. Trigger: a
permission error at launch, or the tier path momentarily holding a non-tier file (`readHeader` →
`invalid magic`, reproduced in a probe). The view reports "History unavailable" until the TUI is
restarted. **Fix:** retry the open on the follow cadence while `source == nil`.

### L-5 · LOW · CONFIRMED · FIXED — `new` — the read-only tests never exercise a live writer, and the "never writes" test misses the interesting variants

> **Status: fixed in `8beb057`.** `TestReadOnlyStoreReadsWhileOwnerWrites` runs a concurrent
> writer and checks every returned value; it fails 5/5 without the scan guard.
> `TestReadOnlyStoreReadsLegacyTierWithoutMigrating` covers a v1 file, and
> `TestReadOnlyStoreNeverWrites` now runs with configs naming 32 MiB and 256 KiB over a 1 MiB tier.

**`internal/storage/readonly_test.go:84-129`** closes the owner before opening read-only and uses
the same `max_size`, so a hypothetical grow/migration on open would still pass. There is no test in
the release that reads a tier while another goroutine/process writes it, although
`docs/dev/05-storage-engine.md:213-215` claims coverage of "following a live writer". Probes show
the gaps concretely: a legacy v1 file is read without migration (codec version 1, bytes identical,
no `.migration`), and a config claiming 32 MiB over a 1 MiB file leaves every file byte-identical.
**Fix:** add a v1 read-only test, a mismatched-`max_size` byte-identity test, and a `-race`
writer-goroutine + reader test that asserts every returned raw value.

### L-6 · LOW · CONFIRMED (mechanism) · FIXED — `new` — the TUI plans and labels History from its own tier config, with no cross-check of on-disk geometry

> **Status: fixed in `a93a829`.** `Store.LayoutMismatch()` compares the tier sizes and the number
> of tier files on disk with the config. The History view shows the difference and asks for the
> service's `--config`. Resolutions still come from the config, so labels are right once the
> configs match.

**`internal/storage/readonly.go:40` → `store.go:466`, `:681-683`, `internal/tui/history.go:551-553`**

Resolutions, bucket steps and retention hints all come from `cfg.Tiers`, never from the header the
file actually carries. Run `kula serve` with non-default intervals/tiers and `kula tui` without the
same `--config` and the History view mislabels its sampling tier and source text (values stay
roughly right thanks to `min()` weighting). **Fix:** compare header `maxData` against
`configs[i].MaxBytes` and warn, or read the resolutions from the header.

---

## Low — tooling, CI and packaging

### L-7 · LOW · CONFIRMED · FIXED — `new` — `ci-local`'s GitHub-expression emulator silently yields wrong values in several documented contexts

> **Status: fixed in `f51def2`.** `run:` scripts see every context that `if:` does.
> `steps.<id>.outcome` and `.conclusion` follow GitHub, including `continue-on-error` and
> `skipped`. `matrix.include`/`exclude` raise `Unsupported` instead of being ignored. `run-all`
> runs jobs in `needs:` order and skips dependents of a failed job. Four new unit tests.

**`addons/ci-local/run-workflow.py:1300-1308` vs `:1188-1190`; `:1361-1364`; `:1117-1128`; `:1132-1136`**

`context.contexts["steps"]` is set for `if:` and `env:` but not copied into the `script_context`
used to interpolate `run:`, so `${{ steps.s.outputs.v }}` inside a script expands to empty while the
same expression in `env:` works (reproduced by the reviewer with a probe workflow). `state.steps[id]`
records only `conclusion`, set to `failure` even under `continue-on-error`, and never records
`outcome` — GitHub sets `outcome=failure, conclusion=success` there. `matrix.include` is not merged,
so `${{ matrix.os }}` with `include:` prints empty. `needs:` is only warned about and the job runs
anyway, so `run-all` can execute a dependent job before its dependency. Latent today (no Kula
workflow uses these constructs), but the tool's purpose is to be trusted for future workflows.
**Fix:** seed `script_context` from the caller's contexts; record `outcome` and apply
`continue-on-error` to `conclusion`; merge `include` or raise `Unsupported`.

### L-8 · LOW · CONFIRMED · ACCEPTED — `new` — the `ci-local` image pins its base only by tag and verifies two of three toolchains

> **Status: not changed, by decision.** A digest pin would fight the configurable
> `CI_LOCAL_UBUNTU` and the tool's `--pull` refresh, and actions/python-versions publishes no
> checksums to verify against. The gap is documented in `addons/ci-local/Dockerfile`.

**`addons/ci-local/Dockerfile:14` (`FROM ubuntu:${UBUNTU_VERSION}`), `:64-78`**

Node (`:95`) and Go (`:120`) are checksum-verified; the Python toolchain is fetched from a
`versions-manifest.json` whose tarball publishes no checksums, and its `setup.sh` executes at build
time and lands on every step's PATH. The repo's own `addons/docker/Dockerfile` pins by digest. No
docker socket and no host mounts limit the blast radius. **Fix:** pin the base by digest and either
accept an operator-supplied digest for the Python build or document the gap beside the other two.

### L-9 · LOW · CONFIRMED · FIXED — `new` — CI never runs for the new packaging and Chart.js tooling

> **Status: fixed in `d289524` and `50c0c9b`.** `addons/packaging/**`, `addons/chartjs/**` and
> `addons/build-chartjs.sh` now trigger CI. The Chart.js paths also trigger the Frontend
> workflow. CI also runs the new `addons/packaging/check_helpers.sh`.

**`.github/workflows/ci.yml:28-39`** (`&code-paths`) lists `cmd/**`, `internal/**`,
`addons/check.sh`, `addons/build.sh`, `addons/ci-local/**` and `ci.yml` — no `addons/packaging/**`,
no `addons/chartjs/**`, no `addons/build-chartjs.sh`; `frontend.yml`'s filters match. A PR that only
touches `replace_chartjs.sh` (self-labelled "status: testing", and it rewrites the template, the
fixture and Go tests) or the npm inputs runs no CI at all — including the new
`python3 -m unittest discover -s addons/ci-local` step. **Fix:** add those paths to the filters, and
`addons/chartjs/**` to the frontend workflow, since a bundle change *is* a frontend regression.

### L-10 · LOW · CONFIRMED · FIXED — `new` — `replace_chartjs.sh`'s apply phase is not atomic

> **Status: fixed in `9afbac5`.** Before the apply phase, the script backs up everything it will
> change. An `EXIT` trap restores the backup if the script stops early (error, Ctrl-C, TERM). A
> forced mid-apply failure left `git status` clean.

**`addons/packaging/replace_chartjs.sh:634-661`** removes the bundle and `addons/chartjs/` before
`apply_patch`/`write_bundle_test` rewrite `index.html`, the perf fixture and the tests. Phase 1
guarantees the substitutions match, so a Ctrl-C/OOM in between leaves a tree with three new copies,
no bundle, and a template still pointing at it — a broken build until the script is re-run (which
does recover; verified by the reviewer). **Fix:** copy the trio into place after the patches, or
stash the removals behind a trap.

---

## Low — release hygiene and docs

### L-11 · LOW · OPEN — the CHANGELOG's `Fixed` list omits the last three user-visible fixes before the tag

> **Status: open, left to the maintainer.** At `2a7f63e` the section is still `[Unreleased]` and
> still lacks bullets for `918647a`, `25c94bc` and `999e01e`. It also needs entries for the
> fixes to this review's findings.

The `[Unreleased]` → `Fixed` block (`CHANGELOG.md:35-44`) covers the Focus Mode translation, Space
Invaders on Firefox, the layout shifts and the light-theme first frame. Commits landed *after* the
last changelog update (`4d491ce`) that users will notice are missing: `918647a` (System Info no
longer grades fans/voltages with temperature thresholds; uptime now refreshes — the two findings
from the 0.20.0 review), `25c94bc` (the remaining hard-coded dashboard strings are now translated:
Graph Bounds, Y-Axis Limit, Apply, cores, connection status, the AI button), and `999e01e`
(system-info tab layout). **Fix:** add one `Fixed` bullet per item before renaming the section.

### L-12 · LOW · FIXED — `pre-existing` — 57 English keys are still absent from all 25 non-English locales

> **Status: fixed in `f157feb`.** All 57 keys are translated in all 25 locales.
> `TestCurrentUITranslationsCoverEveryLocale` now requires every `en.json` key in every locale and
> rejects keys that `en.json` no longer has. All 26 locales now carry exactly 391 keys.

`internal/i18n/locales/en.json` has 391 keys; every other locale is missing the same 57 (11 of
which the dashboard actually looks up, e.g. `history_back`, `history_forward`, `zoom_out`,
`live`, `updated`, `tier`, `pinned_time`). The dashboard falls back to English
(`internal/i18n/i18n.go:65-78`, `i18n.js` `t()`), so users see untranslated strings rather than
missing ones, and `TestCurrentUITranslationsCoverEveryLocale` deliberately enforces only the `si_*`
set plus a fixed list. This is the 0.20.0 review's M-3 carried over: **this release did the right
thing for its own strings** — all 26 keys added to `en.json` since 0.20.3 are present in all 26
locales (verified programmatically), which is a clear improvement. The backlog itself is unchanged.

### L-13 · LOW · FIXED — the docs tree still advertises version `0.20.0`

> **Status: fixed in `acaa053`.** Both markers now say `0.21.0`.

`docs/README.md:73` ("documents the codebase at version `0.20.0`") and
`docs/user/11-prometheus.md:133` ("This list reflects version `0.20.0`") while `VERSION` is already
`0.21.0`. Both are release-time markers; bump them with the tag.

---

## Previous review's findings

The two medium findings from the 0.20.0 review are **fixed** in this release, exactly as suggested:

| 0.20.0 finding | Status in 0.21.0 |
|---|---|
| M-1 — System Info graded every sensor with temperature thresholds (fans at 1240 RPM → "critical") | **Fixed** — `sensorSeverity()` now gates on `sensorKind(sensor) === 'temperature'` (`system-info.js:733-741`, used at `:769`) and rows are keyed by device+name (`:743-750`) |
| M-2 — System Info Overview never refreshed uptime | **Fixed** — uptime is now a live-patched fact (`system-info.js:298`, `data-live="uptime"`) |
| M-3 — new UI strings shipped English-only in all 25 locales | **Improved** — all 26 keys added in this release exist in all 26 locales; the older 57-key backlog remains (L-12, since fixed in `f157feb`) |

---

## What holds up

**Release gate, fuzzing and the shipped binary.** `./addons/check.sh` passes end to end (gofmt, vet,
`-race` tests in all 12 packages, golangci-lint `0 issues`) and govulncheck reports "No
vulnerabilities found". All 15 fuzz targets survived 20 s each, including the two new minifier
targets. `./addons/test-frontend-regressions.sh` drives the real binary in Chromium and passes both
fixtures with `"errors":[]`, including the new PNG export, the clock picker, the calendar range,
system-info grading, session-expiry re-login and the 46-chart/165,600-point performance fixture — so
the minified assets are exercised end to end in a browser, not just in unit tests. The black-box
scanner reports **47 pass / 0 fail** in `-aggressive -fuzz` mode against a correctly-configured live
instance (the single warn is plaintext HTTP with no TLS listener, the two skips are the absent TLS
listener and Ollama being disabled). An earlier 10 s `-dos-wait` run flagged `DOS-SLOWLORIS`; that is
a probe artifact — the server sets `ReadTimeout: 30 s` (`server.go:517`) and the check passes with
the default 35 s wait. The server log shows only expected WebSocket origin rejections from the CSWSH
probe.

**Read-only storage (the TUI History view).** The claim in `cmd/kula/main.go:287-289` — the TUI
never writes, migrates or resizes storage it does not own — holds for the tier files. `OpenReadOnly`
only `Stat`s and `Open`s; a missing directory/file or a <64-byte file reads as empty; a legacy v1
file is decoded without migration (codec version stays 1, bytes identical, no `.migration`); a
config claiming a different `max_size` leaves every byte untouched; a 0555 directory with 0444 tiers
supports refresh, query, snapshot and close. `WriteSample`/`Tier.Write`/`Tier.Flush` return
`ErrReadOnly` before touching anything, `QueryLatest` returns nil, and the TUI only calls
`Refresh`/`QueryRangeWithMeta`/`RetainedRanges`, so the error cannot reach the UI. Robustness
probes: 8 corruption classes × 3 rounds produced no panic and no hang; 50,472 samples read while the
owner wrote 47,759 at full speed produced **0 wrong values, 0 nil payloads, 0 query errors** (only
L-3's ordering); 21,491 open+refresh cycles against a wrapping 4 KB ring produced no errors; 200
owner restarts produced no fd leak (8 → 6). 300 randomised history datasets rendered frame-exact
(height × width) with no panic, and the 30-day worst case renders in ~2.3 ms.

**The minifier and static-asset pipeline.** The fallback contract works: an asset that fails to
minify is logged and served unminified, and SRI (`server.go:1114-1136`) and the ETag
(`:1149-1172`) both hash `readStatic`, so the integrity tag always describes the served bytes.
Minification is deterministic (three evaluations, identical SHA-256 over all 37 assets) and
measurably does what the README claims: the server logs `Minified 37 static assets: 727 KiB -> 490
KiB` — a third smaller. ASI rules hold under probe (`return\nx`, `a\n++b`, `a=b\n(c)` keep their
breaks), and all HTML-like-comment hazards are guarded. CSS whitespace drops are delimiter-adjacent
by construction; instrumenting the shipped sheets found 7,583 + 1,453 drop sites with no token-merging
pair, and the descendant-combinator space survives. Beyond the repo's own tests I minified the three
real upstream bundles the distro path installs (chart.umd.js, chartjs-plugin-zoom.js,
chartjs-adapter-date-fns.bundle.js) and diffed every literal/string/template/regex value with acorn
8.18.0: minification succeeded, `node --check` passed and the literal streams were **identical**.

**Static serving, ETag and fonts.** A live instance answers `200` with
`Etag: W/"…"`, `Cache-Control: no-cache`, `Vary: Accept-Encoding` and revalidates with
`If-None-Match` to `304` (verified with curl); the 304 path drops `Content-Encoding` and skips the
gzip writer (`server.go:176-190`), and a 304/204 can never ship gzip framing. WOFF2 is served as
`font/woff2` and excluded from gzip. The Inter `.ttf` is gone with no dangling reference (repo-wide
grep), the WOFF2 header is self-consistent, and the packaging script that removes fonts is
format-agnostic. Every `{{sri …}}` reference in `index.html` resolves to a real file (checked
programmatically), so no integrity attribute silently renders empty.

**Frontend security.** The new modules introduce **no HTML-string sink**: `chart-image.js`,
`clock-picker.js`, `tv-mode.js` and `format.js` build DOM with `createElement`/`createElementNS` +
`textContent`/`setAttribute`, and the release diff adds no `innerHTML`/`insertAdjacentHTML`/`eval`/
`new Function` anywhere. The pre-paint theme script carries the per-response CSP nonce
(`index.html:58`, asserted by `static_assets_test.go:107-130`). PNG export cannot taint or leak: the
source is a same-origin Chart.js canvas, nothing in `js/app/` draws an `Image`/video, and filenames
are slugged to letters and digits, so `../`, quotes and `:` cannot reach `anchor.download`. TV mode
binds document listeners once and removes them with the matching capture flag, disconnects both
observers, guards the `tv-fill` mutation loop, and wraps every `localStorage` and wake-lock call. The
`state.js` removals (`selectedVram`, `selectedGpuTemp`, `colors.tealAlpha/limeAlpha`) have zero
remaining references. No new per-card document listeners were added, and the chart-card buttons stay
idempotent.

**Tooling security.** All workflows are first-party, SHA-pinned, `permissions: contents: read`, with
no `pull_request_target` and no `secrets.*` anywhere; `ci.yml` even fails if checkout leaves
credentials behind. `ci-local` passes only version strings and user `-e` vars into the container, mocks
the token/secret contexts as empty, mounts no docker socket and no `$HOME`, uses a per-run `mktemp`
snapshot with EXIT/INT/TERM cleanup, and strips tokens out of a repo URL before using it as a slug.
`build-chartjs.sh` uses `mktemp` + a quoted trap, and every npm invocation carries
`--ignore-scripts --min-release-age --no-audit --no-fund` with exact pins; re-running it reproduced
the committed bundle byte-for-byte and re-verified all 32 locked packages against the live registry.
`replace_chartjs.sh` downloads with pinned sha256 hashes (re-downloaded and recomputed: all three
match), refuses a corrupted pin without touching the tree, and its `--check` mode writes nothing.
`python3 -m unittest discover -s addons/ci-local` → 15 passed; `shellcheck -S warning` clean.

---

## How this was verified

- **Static gate**: `./addons/check.sh` with `HOME`/`GOPATH`/`GOCACHE` redirected into writable space
  — `gofmt -l` clean, `go vet ./...` clean, `go test -v -race ./...` ok for all 12 packages,
  golangci-lint `0 issues`; `~/go/bin/govulncheck ./...` → no vulnerabilities found.
- **Fuzzing**: `./addons/fuzz.sh 20s` — 15/15 targets survived (FuzzMinifyJS 629,228 execs,
  FuzzMinifyCSS 574,302 execs, FuzzValidateOrigin 412,572, FuzzGetClientIP 610,522).
- **Browser**: `./addons/test-frontend-regressions.sh` → pass, both fixtures, `"errors":[]`,
  `chart_image_export:true`, `custom_picker:true`, `calendar_range:true`.
- **Black box**: built both binaries from this tree, served a config with auth + Prometheus token +
  minification on, confirmed login (`200`, CSRF token, `Set-Cookie`) and revalidation (`304`), then
  ran `kula-scan` in default mode (32 pass / 0 fail / 1 warn / 17 skip) and
  `-aggressive -fuzz -fuzz-iter 200` (47 pass / 0 fail / 1 warn / 2 skip).
- **Minifier**: temporary probe test (created, run, deleted) exercising `minifyJS` on the M-1 inputs
  and on the three upstream bundles fetched from unpkg; acorn 8.18.0 parsed both sides and compared
  every `Literal`/`TemplateElement` value; `node --check` on each output.
- **Supply chain**: reproduced M-2 with a synthetic `lockfileVersion: 1` lock and with a root
  `optionalDependencies` entry absent from `packages`.
- **Storage/TUI**: independent probes in a copy of the tree for the corrupt-length allocation,
  read-only byte-identity (v1 file, mismatched `max_size`, 0555 directory), 8 corruption classes,
  a full-speed writer with 50 k samples, 21 k open+refresh cycles, fd accounting, and 300 randomised
  render frames.
- **i18n**: programmatic comparison of `en.json` against 0.20.3 and against all 25 other locales,
  plus a scan of every `data-i18n*` attribute and `t('…')` call site for missing keys.
- **Workspace**: the tree is byte-identical to how I found it — scratch directories were removed and
  the only file added is this review.

**Environment notes for the maintainer** (not repo defects): in this sandbox `$HOME` is read-only,
so `go test ./internal/config/` fails with "insufficient permissions to create data storage" until
`HOME` is redirected to a writable directory, and the Go build cache must be pointed somewhere
writable (`GOCACHE=/tmp/...`). With that redirection every stage passes. The same note appeared in
the 0.20.0 review.

---

## Suggested order

> **Status at `2a7f63e`:** items 1–5 are done. Of the rest, only **L-11** (release notes) and the
> JS half of **L-1** remain open, and L-8 was accepted. Each finding's status note has the details.

1. **M-1** — one-line `"default": true`, an `await`-aware `for` head, and two test cases; this is the
   only finding with a silent-semantics failure mode, and it is cheapest to close now.
   **Done** in `d538ddc`.
2. **M-3** — clamp the TV-mode row height to the stylesheet's `10rem` floor; it is the headline
   feature of the release and the failure is visible on wall displays.
   **Done** in `7577d21`.
3. **M-2** — require a `packages` map and check every dependency kind in `verify-lock.js`; pair with
   **L-9** so lockfile changes are covered by CI at all.
   **Done** in `03ac3ac`, with L-9 in `d289524` and `50c0c9b`.
4. **M-4** — give the TUI its own custom-metrics socket (or refuse to unlink a live one); the
   release now advertises the TUI-alongside-serve workflow.
   **Done** in `8778664`.
5. **M-5** — clamp the record length to the real file size; cheap, and it closes a memory-spike path
   the new read-only reader inherits.
   **Done** in `8beb057`.
6. **L-1, L-2, L-3** — test strength for the minifier, the DST mismatch, and ordering in the TUI
   plot. L-2 and L-3 **done**; L-1 **partly done** (CSS half).
7. **L-11, L-13** — finish the release notes and the two doc version markers before the tag.
   L-13 **done**; L-11 **open** (maintainer).
8. **L-4 … L-10, L-12** — backlog; none blocks the tag.
   All **done** except L-8 (accepted).
