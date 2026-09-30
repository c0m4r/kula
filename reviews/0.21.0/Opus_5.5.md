# Kula 0.21.0 — pre-release code review

- **Reviewer:** Claude Opus 5.5
- **Date:** 2026-09-30
- **Scope:** `0.20.3..f412f62` (`main`, VERSION `0.21.0`): 46 commits, 129 files, +14 252 / −1 000
- **Method:** I read the new and changed code, ran every gate, and reproduced each finding in scratch
  copies of the tree (`git archive`), so the repository was never modified. Part 1 was written before
  I opened the other review in this directory. Part 2 checks that review's findings against the code.

## Verdict

**Ready to release after one packaging fix.** (Part 2 adds three pre-tag items from the other review.) The runtime changes are solid. Minification, ETag
revalidation, the deferred Chart.js bundle, the native date adapter, TV mode, the clock picker,
chart PNG export and the read-only TUI history all behave as documented, and every gate passes.

- **Fix before tagging:** M-1. Both packaging helpers marked *stable* now leave a failing
  `go test ./internal/web/`. It worked in 0.20.3.
- **Worth fixing soon, but nothing ships broken today:** L-1 (a JS minifier miscompile that no
  shipped script triggers yet) and L-2 (the TUI history can show a wrong point during a torn read).

> **Status at `2a7f63e`:** every Part 1 finding is fixed except **I-1** (the CHANGELOG, left to the
> maintainer) and **I-2** (accepted, and documented in the Dockerfile). Each finding below names the
> commit that fixed it. [Part 3](#part-3--resolution) covers both reviews in this directory.

## Verification performed

| Check | Result |
|---|---|
| `./addons/check.sh` (gofmt, vet, `go test -race ./...`, golangci-lint) | pass, golangci-lint 0 issues. govulncheck is not installed, so the script skipped it |
| `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` | 0 reachable vulnerabilities. Informational: GO-2026-5932 (`x/crypto/openpgp`, not imported) |
| `./addons/test-frontend-regressions.sh` (Node 22+, Chromium) | pass: dashboard, performance and live suites, including `chart_image_export`, `custom_picker`, `calendar_range` |
| `replace_chartjs.sh --download` on a clean copy, then `go test ./internal/web/` and the browser regressions | pass: the pinned sha256 values match, and the upstream trio passes every browser suite |
| `remove_fonts.sh` / `remove_game.sh` on a clean copy, then `go test ./internal/web/` | **fail** (M-1); the same steps pass on `0.20.3` |
| `python3 -m unittest discover -s addons/ci-local` | pass (15 tests) |
| Live `kula serve` probed with curl | `W/"…"` ETag, `Cache-Control: no-cache`, bodiless 304 with `Content-Encoding` removed, `Vary: Accept-Encoding`, WOFF2 not gzipped, `If-None-Match: *` → 304, `data-default-theme` rendered, `Minified 37 static assets: 727 KiB -> 490 KiB` |
| Minifiers run on hand-written edge cases (scratch copy of `minify.go`) | JS: all ASI cases preserved, plus one miscompile (L-1). CSS: two latent rewrites (L-3) |
| Concurrent writer + `OpenReadOnly` reader stress test (scratch copy) | 13–18 wrong samples per 1.25–1.54 M records scanned, in each of four runs (L-2) |
| `go run` / `-gcflags=-N` probe of `return m, m.mutate()` | gc runs the call first, so the code works today (L-4) |

## Findings

| ID | Severity | Area | Title | Status |
|---|---|---|---|---|
| M-1 | Medium | packaging / tests | `remove_fonts.sh` and `remove_game.sh` now leave `go test ./internal/web/` failing | **Fixed** `d289524` |
| L-1 | Low (latent miscompile) | `internal/web/minify.go` | A keyword used as a property name is lexed as the start of a regex; the minifier can then fold the next statement into a comment | **Fixed** `d538ddc` |
| L-2 | Low | `internal/storage/readonly.go` | A torn read can return wrong or out-of-order samples, not only skip them as the code comment says | **Fixed** `8beb057` |
| L-3 | Low (latent) | `internal/web/minify.go` (CSS) | The CSS minifier has no verification pass; two selector rewrites change meaning | **Fixed** `d538ddc` |
| L-4 | Low (fragility) | `internal/tui/tui.go` | `return m, m.refreshHistoryIfStale()` depends on an evaluation order the Go spec leaves open | **Fixed** `a93a829` |
| N-1 | Nit | `clock-picker.js` | The hold-to-repeat timer is not stopped when the picker closes | **Fixed** `0e7eaac` |
| I-1…I-4 | Info | various | CHANGELOG heading, unverified Python toolcache download, tier config trust, unguarded `localStorage` | I-3 **fixed** `a93a829`, I-4 **fixed** `2a7f63e`; I-1 open (maintainer); I-2 accepted |

---

### M-1 · Medium · FIXED — the packaging helpers now break the web test suite

> **Status: fixed in `d289524`.** The font and stylesheet tests walk the embedded tree for
> stylesheets and WOFF2 files, and skip whatever a helper removed. The new
> `addons/packaging/check_helpers.sh` runs `remove_fonts.sh` and `remove_game.sh`, alone and in both
> orders, on scratch copies, then requires `go vet ./...` and `go test ./internal/web/` to pass there.
> CI runs it.

**Where:** `internal/web/static_assets_test.go:22` (`TestStylesheetFontsAreEmbedded`),
`:41` (`TestWOFF2FontServedWithoutGzip`), `:140`/`:155` (`TestStaticAssetsRevalidate`), and
`addons/packaging/remove_fonts.sh` / `remove_game.sh`.

The new tests require font files and `game.css` to exist. The two helpers, both documented as
*stable* and *"safe to run in any order"* (`addons/packaging/README.md:5`), delete exactly those files:

| Tree | Failing tests |
|---|---|
| HEAD + `remove_fonts.sh` | `TestStylesheetFontsAreEmbedded` ("references no fonts"), `TestWOFF2FontServedWithoutGzip` (`GET font = 404`), `TestStaticAssetsRevalidate` (file does not exist) |
| HEAD + `remove_game.sh` | `TestStylesheetFontsAreEmbedded` (`open static/game.css: file does not exist`, `t.Fatal`) |
| HEAD + both, either order | all three above |
| `0.20.3` + either script | none: `ok kula/internal/web` |

Both scripts exit 0, so the failure first appears when the package build runs its test step.
Distro packaging (the audience of these scripts) usually runs one, so a Debian, RPM or AUR build
using `remove_fonts.sh` fails.

**Fix:** make the tests adapt to the tree rather than hard-code it:
- `TestStylesheetFontsAreEmbedded`: skip a missing stylesheet, and allow a sheet with no
  `url('fonts/…')` when `static/fonts` is absent.
- `TestWOFF2FontServedWithoutGzip` / `TestStaticAssetsRevalidate`: find the first embedded
  `*.woff2` with `fs.WalkDir`, and `t.Skip` or drop the font case when there is none.
- Alternatively, have the scripts patch these tests, the way `remove_game.sh` already strips the
  game-only tests. Adapting the tests is sturdier.
- Add "run `go test ./internal/web/` after each helper" to CI, or at least to the release checklist.

### L-1 · Low (latent miscompile) · FIXED — a keyword-named property can make the minifier drop a statement

> **Status: fixed in `d538ddc`.** The lexer marks a word that follows `.` or `?.` as a property
> name, and `jsRegexAllowed` treats it as an operand, so a following `/` is division. The
> `x = a.of / 2 // halve a/b,⏎y()` input is now a `TestMinifyJS` case and a fuzz seed. The
> `{} / 2` heuristic keeps its old behaviour, with a code comment, as recommended.

**Where:** `internal/web/minify.go:368-378` (`jsRegexAllowed`) with `jsKeywordsBeforeExpression`
(`:110`).

A `/` after a word in `jsKeywordsBeforeExpression` is lexed as the start of a regular expression,
even when that word is a **property name**: `a.of / 2`, `stats.in / t`, `x.new / y`. Usually the
mis-lexed span is copied verbatim and nothing changes. If a `//` comment follows and holds a `/`,
though, its contents are lexed as tokens. When the comment ends in a punctuator, the line break
after it counts as removable, so the next line is pulled into the comment:

```
input : x = a.of / 2 // halve a/b,⏎y()
output: x=a.of/ 2 //halve a/b,y()        ← y() is now inside the comment
```

Confirmed with the real `minifyJS` and Node: the original calls `y()` once, the minified text
never. Neither safety net catches it. The re-lex check uses the same lexer, so both passes agree,
and `TestMinifiedScriptsParseInNode` only checks syntax, which is still valid. No shipped script has
this pattern today (grep of `internal/web/static/**/*.js` for a keyword property followed by `/`),
which is why the severity is low. Still, the default is `minify_assets: true`, and a future
`net.in / …` line with a trailing comment would silently remove code.

**Fix:** in `jsRegexAllowed`, a word whose previous token is `.` or `?.` is a property name, so
treat it as an operand (division), not a keyword. Record that in the token during lexing, e.g. a
`property` flag set when the previous token is `.`/`?.`. Add `x = a.of / 2 // a/b,\ny()` and
`a.in / b` to `TestMinifyJS` and to the fuzz seeds. The `}`-ends-a-block heuristic has the same
shape (`x = {} / 2 // a/b,\ny()` also swallows `y()`), but real code essentially never divides an
object literal, so a comment is enough there.

### L-2 · Low · FIXED — the read-only history can show wrong or out-of-order points while the service writes

> **Status: fixed in `8beb057`** (fix 2 below), with fix 3 on the plot side in `a93a829`.
> `readOnlyScan` re-reads the owner's header every 256 records, before each batch and at the end.
> A scan that the owner overtook fails with `errHistorySnapshotExpired`, and the store retries it
> up to three times on a fresh header. `TestReadOnlyStoreReadsWhileOwnerWrites` runs a concurrent
> writer and checks every returned value. It fails 5/5 without the guard. The doc comment and
> `docs/dev/05-storage-engine.md` now describe the retry. The TUI's `inTimeOrder` also drops any
> bucket that does not follow the previous one.

**Where:** `internal/storage/readonly.go:24-29` (doc comment), `internal/storage/tier.go:410-413`
(a record is written with two `WriteAt` calls), `scanRange`.

The comment says a scan racing the writer *"can end that segment early or skip a torn record"*.
It can also **return** a torn record. Records carry no checksum. When the reader's header snapshot
still points at bytes the writer has since overwritten, a new length prefix or timestamp can pair
with the tail of the old record and still decode.

Reproduced with a scratch test: one `NewStore` owner writing continuously into a 16 KiB tier, and
one `OpenReadOnly` reader looping `Refresh()` plus `tiers[0].ReadRange` over the whole ring. Each
sample carries `usage = i mod 97`. Four 8-second runs returned **15, 18, 16 and 13 wrong samples out
of 1.25–1.54 M**, for example `ts+584s want=2 got=39`, with **timestamps newer than the snapshot's
own newest record**. So a torn read can inject an out-of-order point as well as a wrong value.

**Impact:** TUI display only, it corrects itself on the next refresh, and it needs a window that
reaches a tier's oldest retained records while the owner overwrites them. That happens mostly after
panning back to the retention limit, or when a window roughly equals a tier's retention. With
real ring sizes and one write per second it is rare, but not impossible.

**Fix, in order of effort:**
1. Correct the comment: results may include torn samples.
2. Seqlock-style validation. After the scan, re-read the header. If the writer's `writeOff`
   advanced across bytes the scan read from the old segment, or `count` dropped, drop the samples
   from that byte range, or redo the query once. This is the cross-process counterpart of the
   in-process `writeCycle`/`writeEnd` check.
3. Cheap guard: drop any sample whose timestamp is not strictly after its predecessor within a
   segment, or later than the snapshot's `newestTS`. That catches the out-of-order cases observed.

### L-3 · Low (latent) · FIXED — two CSS rewrites change a selector's meaning

> **Status: fixed in `d538ddc`.** A hex escape keeps its terminating whitespace. A comment is
> dropped only beside a delimiter, and otherwise stays as `/**/` (so `1px/**/2px` survives). Both
> inputs are `TestMinifyCSS` cases and fuzz seeds. `TestEmbeddedAssetsMinify` now checks
> stylesheets with `cssGapsPreserved`, which shares no code with the minifier, so the CSS side
> finally has an independent verification pass.

**Where:** `internal/web/minify.go:566+` (`minifyCSS`): escape handling at `:621`, comments at
`:607`.

The JS minifier re-lexes its output. The CSS minifier has no equivalent check, and its fuzz target
only checks that the output minifies again. Two inputs change meaning:

| Input | Output | Effect |
|---|---|---|
| `.a\31  .b{}` (hex escape, its terminating space, then a descendant space) | `.a\31 .b{}` | the one remaining space is taken as the escape terminator, so the descendant combinator disappears (`.a1.b`) |
| `.a/**/.b{}` | `.a .b{}` | a comment becomes whitespace, turning a compound selector into a descendant selector |

Neither pattern occurs in `style.css` or `game.css`. The only escape (`content: '\200b'`) is inside
a string, so nothing is affected today.

**Fix:** after a hex escape, keep one extra space when the source had two or more. Replace a comment
with nothing when neither side has whitespace, but keep `/**/` when both neighbours are
identifier-like, since `1px/**/2px` must not become `1px2px`. Add both inputs to `TestMinifyCSS`.

### L-4 · Low (fragility) · FIXED — the Go evaluation order in the TUI `Update` returns is unspecified

> **Status: fixed in `a93a829`.** Every call site now runs `command := m.refreshHistoryIfStale()`
> before `return m, command`.

**Where:** `internal/tui/tui.go:281, 343, 346, 361`: `return m, m.refreshHistoryIfStale()`.

`refreshHistoryIfStale` has a pointer receiver and mutates `m.history` (`requestID++`,
`loading = true`). The Go spec leaves open whether the operand `m` is read before or after the call
in the same return list (compare the spec's `[]int{a, f()}` example). gc runs the call first
(verified, also with `-N`), so the code works today. If `m` were read first, the returned model
would keep the old `requestID`, every `historyLoadedMsg` would look stale and be discarded, and the
History view would never load.

**Fix:** `cmd := m.refreshHistoryIfStale(); return m, cmd`. Line 291 (`append(commands, …)` and then
`return m, tea.Batch(commands...)`) is already safe.

### N-1 · Nit · FIXED — the clock picker's hold-to-repeat can outlive the picker

> **Status: fixed in `0e7eaac`.** A `pointerup` or `pointercancel` anywhere in the window stops the
> repeat, and a pending tick does nothing once the picker is no longer rendered.

**Where:** `internal/web/static/js/app/clock-picker.js:196-219`. Found by reading the code, not
reproduced in a browser.

Holding `+`/`−` on a wheel starts a `setTimeout` loop. Only `pointerup`/`pointercancel`/
`pointerleave` on `wheelHost` and `pointerout` from the step button stop it. If the picker closes
while the button is held (Esc), the release may land outside the now-hidden host. The loop then
keeps calling `onPick` on the hidden draft every 80 ms. Stopping the repeat on a `pointerup` at the
document level, or when the pane hides, closes the gap.

### Informational

- **I-1 · OPEN (maintainer)** `CHANGELOG.md:10` is still `## [Unreleased]`; it needs the `0.21.0` heading and date
  before tagging. *Unchanged at `2a7f63e`; the maintainer writes the CHANGELOG.*
- **I-2 · ACCEPTED** `addons/ci-local/Dockerfile:60-78` installs the actions/python-versions tarballs with only
  TLS protecting them. This is documented ("The manifest publishes no checksums") and is the same
  trust setup-python uses. Node and Go downloads are sha256-checked. Acceptable for a dev tool.
- **I-3 · FIXED (`a93a829`)** `OpenReadOnly` trusts the TUI's own `storage.tiers` config (tier count, resolutions for
  labels and tier choice). A TUI run with a different config than the service reads the wrong
  number of tiers or mislabels resolutions. The History view and docs tell users to pass the
  service's `--config`, which is enough; a warning when a header's geometry disagrees would be
  friendlier. *Now `Store.LayoutMismatch()` compares tier sizes and the number of tier files, and
  the History view shows the difference and asks for the service's `--config`.*
- **I-4 · FIXED (`2a7f63e`)** (pre-existing) `state.js` reads `localStorage` without a guard when the module loads, so
  blocked storage stops the dashboard. The new pre-paint script in `index.html` does guard it; the
  modules could do the same. *Now every dashboard module goes through the new `js/app/prefs.js`
  (enforced by `TestDashboardStorageGoesThroughPrefs`), `game.js` has its own guarded helpers, and
  the live-dashboard browser test loads both with every `localStorage` access throwing.*

## Reviewed with no findings

- **Static serving:** ETag over the served (minified) bytes, weak comparison, `*`, 304 through
  the gzip wrapper (no gzip framing, `Content-Encoding` removed), `Vary`, WOFF2 excluded from gzip,
  `no-cache`. SRI is computed over the minified bytes, and vendored `*.min.*` files are not minified.
- **JS minifier otherwise:** token-stream re-lex guard, idempotence fuzzing, ASI handling (`return`,
  postfix `++`/`--`, `)`/`]`/`}` followed by `(`/`[`/template, `<!--`/`-->`, `a ? .5 : b`). All my
  ASI cases were preserved.
- **Pre-paint theme script:** has a nonce, `html/template` escapes the attribute, and it matches
  `state.js`'s key and fallback order.
- **Deferred Chart.js:** a deferred classic script runs before the module scripts that follow it in
  document order. The upstream trio from `replace_chartjs.sh` keeps the order and `defer`.
- **Native date adapter** (`addons/chartjs/date-adapter.js`): `addMonths`, `startOfWeek`, `endOf`,
  and the whole-day and whole-month differences match date-fns' algorithms. Only the listed format
  tokens are used (`charts-init.js:153-159`).
- **`verify-lock.js` and `build-chartjs.sh`:** exact pins, registry and `sha512` checks, a publish
  age check on every locked package (scoped names included), and `ignore-scripts`.
- **Read-only store:** files are opened `O_RDONLY`, so writes are impossible at the OS level. Every
  write path returns `ErrReadOnly`, a missing directory creates nothing, a replaced inode is
  reopened, and `Close` never writes a header.
- **TUI History:** at most one load in flight with a queued follow-up, stale responses dropped by
  `requestID`, a back-off on failure, a paused view that stops following, gaps drawn as breaks, and
  no divide-by-zero or negative `strings.Repeat` in the chart or frame-centring code.
- **Frontend:** TV mode (layout observer can't loop, wake-lock and listener cleanup), focus-bar
  i18n, chart PNG export (hostname and title slugged into the filename, object URL revoked), and
  the calendar/clock rewrite. All 26 locales carry the new keys, `calendar_year` is gone from every
  locale, and the removed `state` fields have no remaining users.
- **Space Invaders:** graphics settings are validated against enums when loaded from
  `localStorage`, and the level-up timer is cleared on restart.
- **ci-local:** snapshot is private (0700 directory, mktemp name, removed on exit), the repository
  slug is taken from the remote URL without any embedded token, versions are validated before they
  reach tags and build args, resource limits apply, and YAML is read with `safe_load`.

## Disclosure

Before Part 1 was finished, a repository-wide `grep` for `replace_chartjs` matched three lines of
`reviews/0.21.0/DeepSeek_v4.1_Flash.md`. They were a finding title about `replace_chartjs.sh`'s
apply phase not being atomic, plus two sentences about that script. I had already exercised
`replace_chartjs.sh` and did not pursue that topic in Part 1. All later searches excluded
`reviews/`.

---

# Part 2 — checking `DeepSeek_v4.1_Flash.md`

I read the other review only after Part 1 was written, then re-checked every finding against the
code, reproducing it where that was practical. **All 18 findings hold.** Three need a factual
correction and several a different severity. Two of the other review's "holds up" claims conflict
with what I measured.

## Finding-by-finding

| Their ID | Verdict | How I checked | Notes |
|---|---|---|---|
| **M-1** minifier drops whitespace inside a regex after `export default` / `for await (…)` | **Confirmed** | Ran both inputs through `minifyJS`: `export default/[a-z]/;` and `for await(const x of y)/[a-z]/.test(x);`. Node: `/[a-z] /.test("a")` is `false`, `/[a-z]/.test("a")` is `true` | Same root cause as my **L-1**, in the opposite direction: they misread a regex as division, I misread a division as a regex. One fix covers both: property-name awareness, `default`, the `for await` head, and a check that `jsRegex`/`jsString`/`jsTemplate` bytes survive verbatim. **Correction:** the distro path only minifies the upstream trio when `--from` supplies the *unminified* files (`chart.umd.js`, …). `--download` installs `*.min.js`, which Kula never minifies. I'd rate it Low–Medium (latent), and it is cheap enough to fix before the tag |
| **M-2** `verify-lock.js` passes when the lockfile has no `packages` map | **Confirmed** | A `lockfileVersion: 1` lock with an off-registry `resolved` URL, and a v3 lock with a root `optionalDependencies: {"evil": "^1.0.0"}`. Both print `0 locked packages pinned …` and exit 0 | A gate that can pass vacuously is worth fixing. Exploiting it needs a committed lockfile change, so I'd call it Low–Medium |
| **M-3** TV-mode grid ignores the CSS `minmax(10rem, 1fr)` row floor | **Confirmed** | `tvGridShape(16, 1920, 400, 12)` gives 6×3 with 125 px cells, but three 10rem rows need 504 px. `style.css:3301-3302` | Easier to reach than the review says: the Text size setting scales the root font (`settings.js:111-116`), and the floor with it. A 1080p wall display (1900×950 grid) with 16 charts at 200% text gets 4×4 but needs 1156 px, so the bottom row scrolls out of a chrome-less screen. Agree: Medium |
| **M-4** `kula tui` unlinks the running server's custom-metrics socket | **Confirmed (code)** | `collector.go:129-135` starts the custom collector whenever `applications.custom` is non-empty; `custom.go:107` removes the socket path, `:287` deletes it on exit; `main.go:274-276` runs this for the TUI | Pre-existing. Only happens when custom metrics are configured, but 0.21.0's docs now recommend `sudo kula --config /etc/kula/config.yaml tui`, which is exactly the trigger, and as root. Agree: Medium |
| **M-5** a corrupt length prefix drives an allocation up to the tier size | **Confirmed (code)** | `tier.go:558` bounds `dataLen` only by the header's `maxData` and segment size; `:590` allocates before reading. Default tier 0 is 250 MB | Pre-existing. My **L-2** shows the read-only reader does see torn records, so a torn length prefix can reach this path without any on-disk corruption. That makes a TUI memory spike possible, though rare. I'd rate it Low–Medium; the fix (clamp to the file size, cap one record) is cheap |
| **L-1** `TestEmbeddedAssetsMinify` can't see whitespace-significant CSS corruption; the JS half reuses the lexer | **Confirmed** | `minify_test.go:180-196` strips all whitespace on both sides, then compares `lexJS` output with `lexJS` output | My **L-1** and **L-3** are concrete escapes from exactly this blind spot |
| **L-2** DST-gap times shift an hour while the field shows the typed time | **Confirmed** | Real `parseDateTimeInput` under `TZ=America/Santiago` (`00:30` becomes a `01:30` instant) and `TZ=Europe/Warsaw` (`02:30` becomes `03:30`) | The draft keeps the typed text. After Apply, the header shows the shifted instant, so the mismatch is limited to the draft and its duration summary. Low |
| **L-3** a wrap during a cross-process scan returns out-of-order records | **Confirmed, and worse** | My stress test (Part 1, L-2) | Their own probe found "0 wrong values". Mine found **wrong values too**, 13–18 per 1.25–1.54 M raw records, some with timestamps past the snapshot's newest. Their 50 k-sample run would expect about 0.5 wrong values, so seeing none is chance. The fix should cover values as well as ordering (seqlock-style validation), not only sorting in `layout()` |
| **L-4** one failed `OpenReadOnly` disables History for the session | **Confirmed (code)** | `main.go:289-292` keeps only `HistoryErr`; with `source == nil`, `historyStale` is always false | A missing directory doesn't fail the open (it reads as empty), so the trigger is a permission error or a non-tier file at launch. Low |
| **L-5** the read-only tests never run a concurrent writer | **Confirmed** | `readonly_test.go` has no goroutines; `TestReadOnlyStoreNeverWrites` uses the same `max_size` and a closed owner | `docs/dev/05-storage-engine.md:213-215` says the tests cover "following a live writer"; they interleave writes and reads, they don't run them concurrently. My stress harness is the missing test. Low |
| **L-6** the TUI plans and labels History from its own tier config | **Confirmed** | `readonly.go:40` (`configs: cfg.Tiers`) | Same as my **I-3**. Low |
| **L-7** ci-local expression emulator gaps | **Confirmed** | Probe via `importlib`: `${{ steps.s.outputs.v }}` interpolates to `"echo "` in a `script_context`-like context (`run-workflow.py:1301-1306` never copies `steps`). `needs` only warns (`:1132-1136`). `conclusion` ignores `continue-on-error` and there is no `outcome` (`:1361-1364`). `matrix.include` is not merged | Latent; no Kula workflow uses these constructs. Low |
| **L-8** ci-local base image pinned by tag, Python not checksummed | **Confirmed** | `addons/ci-local/Dockerfile:14` vs digest-pinned `addons/docker/Dockerfile:1,17` | Overlaps my **I-2**. Low |
| **L-9** CI never runs for `addons/packaging/**`, `addons/chartjs/**`, `addons/build-chartjs.sh` | **Confirmed** | `ci.yml:28-39`, `frontend.yml:14-23` | Adding the paths alone would not have caught my **M-1**, since no workflow runs the packaging helpers. A job that runs each helper on a scratch copy and then `go test ./internal/web/` would. Low |
| **L-10** `replace_chartjs.sh` apply phase is not atomic | **Confirmed** | `replace_chartjs.sh:645-655` removes the bundle and `addons/chartjs/` before the patches at `:657-661`; the only trap cleans `$WORK` (`:584`) | Low |
| **L-11** CHANGELOG `Fixed` omits 918647a, 25c94bc, 999e01e | **Confirmed in substance** | `git log` / `CHANGELOG.md:35-44` | **Correction:** 4d491ce is not the last CHANGELOG update. 9004aec (the TUI commit) also edited it, *after* those three commits, and still left them out. Together with my **I-1** (`[Unreleased]` heading) |
| **L-12** 57 English keys missing from all 25 locales (pre-existing) | **Confirmed** | Scripted comparison: 391 `en` keys, exactly the same 57 missing in every locale; all 26 keys added since 0.20.3 present everywhere | **Correction:** "11 of which the dashboard actually looks up" undercounts. The reworked custom-range picker shows `range_dates_required`, `range_start_before_end`, `range_max_31_days`, `range_whole_seconds` and `range_outside_retention` through `i18n.t`, plus `history_empty`/`history_failed`, all in English in every locale. The keys are old, but this release's headline picker is where users now see them |
| **L-13** docs still say `0.20.0` | **Confirmed** | `docs/README.md:73`, `docs/user/11-prometheus.md:133` | Release checklist |
| 0.20.0 review follow-ups (sensor grading, uptime, new-string i18n) | **Confirmed** | `system-info.js` diff (`sensorSeverity` gated on kind, `data-live="uptime"` patched), locale script above | Both fixed as described |

## Where the two reviews disagree

1. **Read-only reader correctness.** The other review's "What holds up" reports "0 wrong values"
   in its concurrent-writer probe. With more samples I measured wrong values (Part 1, L-2), so the
   code comment and `docs/dev/05-storage-engine.md:207-210` understate what a racing scan can return.
2. **Font removal.** The other review calls the font-removal script "format-agnostic" with "no
   dangling reference". That is true of the files, but the new tests in `static_assets_test.go`
   now fail after it runs, and after `remove_game.sh` too (Part 1, **M-1**, reproduced against
   0.20.3 as a regression). The other review did not cover this.
3. **CSS minifier.** The other review says whitespace drops are "delimiter-adjacent by
   construction". That holds for the shipped sheets. For general input it fails with a hex escape
   before a descendant combinator and with a comment between compound selectors (Part 1, **L-3**).
4. **Not covered by the other review:** my **L-1** (a keyword-named property swallows the next
   statement into a comment, a verified miscompile), **L-4** (evaluation order in `tui.go`) and
   **N-1** (clock-picker repeat).

## Its verification claims I re-ran

| Claim | My result |
|---|---|
| `check.sh` passes, golangci-lint 0 issues | Same |
| govulncheck: nothing reachable, one module-level advisory | Same (GO-2026-5932, `x/crypto/openpgp`) |
| `FuzzMinifyJS` / `FuzzMinifyCSS` survive | Same, 20 s each |
| Browser regressions pass | Same, and also on the `replace_chartjs.sh --download` tree |
| `kula-scan` 0 failures | Same. Default mode without auth gives 20 pass / 0 fail / 1 warn / 18 skip; their 32/0/1/17 was with auth and a Prometheus token, so more checks ran |
| `replace_chartjs.sh` pinned sha256 values match | Same |
| ETag, 304, gzip, WOFF2 behaviour | Same (curl against a live server) |

## Combined recommendation

**Before tagging:**
1. **M-1 (mine):** make the static-asset tests tolerate the packaging helpers. This is the only
   finding that breaks a supported workflow today.
2. **Minifier soundness:** their M-1 plus my L-1 together (property-name awareness, `default`,
   `for await`, a verbatim-literal assertion, the four inputs as test cases), plus my L-3 CSS
   cases if time allows.
3. **Their M-3:** clamp TV-mode rows to the computed `10rem` floor.
4. Release notes: `[Unreleased]` → `0.21.0` and the missing `Fixed` bullets (my I-1, their L-11),
   and the two `0.20.0` doc markers (their L-13).

**Soon after:** their M-4 (TUI socket unlink), their M-2 plus L-9 (lockfile gate and CI path
filters, plus a packaging-helper CI job), their M-5 with my L-2 and their L-3 (validate
cross-process scans, bound record allocation), my L-4, then the remaining lows.

---

# Part 3 — resolution

Fixes applied on top of `f412f62` and committed one area at a time (`d289524`…`acaa053`, then
`2a7f63e` for `localStorage`). "Mine" refers to Part 1, "theirs" to `DeepSeek_v4.1_Flash.md`.

| Finding | Commit | Fix |
|---|---|---|
| Mine M-1: packaging helpers break the web tests | `d289524` | The font and stylesheet tests find the embedded WOFF2 and stylesheets in the tree instead of hard-coding them, and skip what a helper removed. New `addons/packaging/check_helpers.sh` runs `remove_fonts.sh` and `remove_game.sh`, alone and in both orders, on scratch copies and requires `go vet ./...` and `go test ./internal/web/` to pass; CI runs it |
| Mine L-1 + theirs M-1: JS regex/division heuristic | `d538ddc` | A word after `.`/`?.` is a property name, never a keyword; `default` precedes an expression; `for await (` opens a loop head. All four inputs are unit tests and fuzz seeds |
| Mine L-3 + theirs L-1: CSS rewrites, weak minify test | `d538ddc` | A hex escape keeps its terminating whitespace. A comment is dropped only beside a delimiter, collapses with adjacent whitespace, and otherwise stays as `/**/`. `TestEmbeddedAssetsMinify` checks stylesheets with `cssGapsPreserved`, which shares no code with the minifier and catches the dropped-space mutations; shipped CSS minifies to identical bytes |
| Mine L-2 + theirs L-3, L-5: torn read-only scans | `8beb057` | `readOnlyScan` re-reads the owner's header every 256 records, before each batch and at the end. The header `count` only grows, so it shows exactly which bytes the owner changed since the snapshot. A scan the owner overtook fails with `errHistorySnapshotExpired`, and a read-only store re-adopts the header and retries up to three times. New `TestReadOnlyStoreReadsWhileOwnerWrites` fails 5/5 without the guard and passes with it (with and without `-race`); the stress harness saw 0 wrong or out-of-order values in 8 M reads. Also tests for a legacy v1 tier and for configs naming another tier size |
| Theirs M-5: allocation bound | `8beb057` | Length prefixes above 64 MiB (`maxRecordBytes`) are corruption, and the writer refuses such records; segment extents are clamped to the file size. The bound was first 16 MiB and was raised before committing: a measured rollup record (Data+Min+Max) is 0.28 MiB for 500 interfaces, 100 filesystems and 200 containers, 12.4 MiB for 20 000/5 000/10 000, and 18 MiB at the codec's 65 535-interface maximum. The on-disk format is unchanged, and the owner's read and write paths change only for records above the bound |
| Mine L-4: evaluation order | `a93a829` | `command := m.refreshHistoryIfStale(); return m, command` |
| Theirs L-4: failed open never retried | `a93a829` | `Options.OpenHistory` replaces the pre-opened store; the TUI retries every 5 s while the History view is shown and closes the store on exit |
| Theirs L-3 (plot side) | `a93a829` | `inTimeOrder` drops buckets that do not follow the previous one, which also covers a recording host whose clock was set back |
| Mine I-3 + theirs L-6: config/layout mismatch | `a93a829` | `Store.LayoutMismatch()` compares tier sizes and the number of tier files; the History view shows the difference and asks for the service's `--config` |
| Theirs M-4: TUI unlinks the custom-metrics socket | `8778664` | The TUI no longer starts the custom-metrics listener, and the collector refuses to unlink a socket another process is serving |
| Theirs M-3: TV grid ignores row floor | `7577d21` | `tvGridShape` takes the stylesheet's `--tv-row-min` (10rem, read in pixels, so it follows the text size) and only considers shapes whose rows fit |
| Theirs L-2: DST gap | `38b09fc` | `existingDateTimeInput` returns the time a value loads as; the picker shows it (unless the field is being typed in, then on blur) |
| Mine N-1: clock repeat | `0e7eaac` | Stopped by a release anywhere and when the pane is no longer rendered |
| Theirs M-2: `verify-lock.js` passes vacuously | `03ac3ac` | Rejects lockfiles older than v2 or without a root entry, requires every kind of direct dependency to be an exact pin with a locked entry, and fails on an empty lock; the real lockfile still passes |
| Theirs L-9: CI path filters | `d289524`, `50c0c9b` | `addons/packaging/**`, `addons/chartjs/**` and `addons/build-chartjs.sh` trigger CI (and the Chart.js paths the Frontend workflow); CI runs `check_helpers.sh` |
| Theirs L-10: `replace_chartjs.sh` not atomic | `9afbac5` | Backs up everything the apply phase changes and restores it if the script stops early (error, Ctrl-C, TERM); a forced mid-apply failure left `git status` clean |
| Theirs L-7: ci-local expressions | `f51def2` | `run:` scripts see every context `if` does; `steps.<id>.outcome`/`conclusion` follow GitHub (including `skipped`); `matrix.include`/`exclude` are rejected as unsupported; `run-all` runs jobs in `needs:` order and skips dependents of a failed job. Four new unit tests |
| Theirs L-12: 57 untranslated keys | `f157feb` | Translated into all 25 locales (1 425 strings, insert-only diff). `TestCurrentUITranslationsCoverEveryLocale` now requires every `en.json` key in every locale and rejects keys `en.json` no longer has |
| Theirs L-13: docs say `0.20.0` | `acaa053` | Both markers now say `0.21.0` |
| Mine I-4: unguarded `localStorage` | `2a7f63e` | New import-free `js/app/prefs.js` (`readPref`, `readJsonPref`, `writePref`, `writeJsonPref`, `removePref`) catches blocked storage, full quotas and corrupt JSON, and keeps a value whose write failed for the rest of the page. Every module except the already-guarded, import-free `tv-mode.js` goes through it (`TestDashboardStorageGoesThroughPrefs`); `game.js` got its own `readStored`/`writeStored`. A Node test covers the helper, and the live-dashboard browser test now loads the dashboard and the game with every `localStorage` access throwing. Against the previous build that phase fails with the dashboard's `SecurityError`, and against the old `game.js` alone it fails on the game |

Not changed:

- **Mine I-1 / theirs L-11 (CHANGELOG):** left to the maintainer, who writes it. It still needs
  the `0.21.0` heading and the `Fixed` bullets for 918647a, 25c94bc and 999e01e, plus entries
  for the fixes above.
- **Theirs L-8 (ci-local base digest, Python checksums):** a digest pin would fight the
  configurable `CI_LOCAL_UBUNTU` and the `--pull` refresh the tool offers, and
  actions/python-versions publishes no checksums to verify against. The gap is documented in
  the Dockerfile.
- **Verbatim-literal assertion for the JS minifier** (the "stronger" fix in theirs M-1, the JS
  half of theirs L-1, and item 2 of the combined recommendation above): not added.
  `TestEmbeddedAssetsMinify` and `FuzzMinifyJS` still compare `lexJS` output with `lexJS` output.
  Every case found so far is now a unit test and a fuzz seed. A new slip in the regex/division
  heuristic, though, would again pass both checks, as the ones found here did. Unlike the CSS
  side, which now has `cssGapsPreserved`, the JS side still has no check that is independent of
  the lexer.

Verified after the fixes: `./addons/check.sh` passes (gofmt, vet, `go test -race ./...`,
golangci-lint 0 issues); `./addons/test-frontend-regressions.sh` passes all three suites;
`./addons/packaging/check_helpers.sh` passes all four helper combinations;
`python3 -m unittest discover -s addons/ci-local` passes 19 tests; `FuzzMinifyJS` and
`FuzzMinifyCSS` pass 20 s each; shellcheck is clean on the packaging scripts; `verify-lock.js`
accepts the real lockfile and rejects the three bad ones.

Re-checked at `2a7f63e` when marking the findings above: each fix listed in this table is present
in the code. `go test -race` passes for `internal/storage`, `internal/web`, `internal/tui`,
`internal/i18n`, `internal/collector` and `cmd/kula`. `verify-lock.js` accepts the real lockfile
(32 packages), the ci-local suite passes 19 tests, and all 26 locales carry exactly `en.json`'s
391 keys.

Docs updated with each fix: `AGENTS.md`, `docs/dev/03`, `05`, `07`, `10`, `11`, `15`,
`docs/user/05`, `06`, `09`, `11`, `docs/README.md` and `addons/packaging/README.md`.
