# Frontend (SPA & TUI)

Kula has two user interfaces: the embedded **web SPA** and the **terminal UI**. Both are part of
the single binary.

---

## Web dashboard (SPA)

Source: [`internal/web/static/`](../../internal/web/static/), embedded via `//go:embed`.

It is a dependency-light, framework-free single-page app built on **Chart.js** (a trimmed
bundle vendored under `js/chartjs/`) with custom SVG/bar gauges. It connects over WebSocket for live
data and falls back to the history REST API for longer ranges.

### Asset layout

```
static/
├── index.html              # main dashboard template (CSP nonce + SRI injected by server)
├── style.css               # dashboard styles
├── kula.svg / favicon.ico  # branding
├── game.html / game.css / game.js   # Space Invaders easter egg
├── fonts/
│   ├── Inter/              # UI font, variable WOFF2 (OFL-1.1)
│   └── Press_Start_2P/     # game font (OFL-1.1)
└── js/
    ├── chartjs/            # Chart.js bundle built by addons/build-chartjs.sh
    └── app/               # Kula's own ES6 modules
```

### Chart.js bundle

`js/chartjs/chartjs-bundle.min.js` is built from [`addons/chartjs/`](../../addons/chartjs/) by
`./addons/build-chartjs.sh` and committed, so building Kula needs no Node.js. It registers only
what the dashboard uses: the line controller, line and point elements, linear and time scales,
Legend, Tooltip and chartjs-plugin-zoom. Hammer.js is left out because pan and pinch use Pointer
Events. Anything else, such as another chart type or the Filler plugin behind `fill`, stays
unavailable until it is registered in `entry.js` and the bundle is rebuilt.

`index.html` loads it with `defer`, so it does not block the first paint. Deferred classic
scripts and module scripts run in document order once parsing ends, so `Chart` is defined before
`main.js` runs; `TestChartJSScriptsDeferred` keeps it that way, and
`addons/packaging/replace_chartjs.sh` defers the distro trio the same way.

The time scale uses Kula's native `Date` adapter (`date-adapter.js`) instead of date-fns. Tick
and tooltip labels come from `Intl` in `format.js`, so the scale only needs local-time calendar
math: unit starts, steps and whole-unit differences.

A distro build can swap the bundle for the upstream UMD trio (`chart.umd.min.js`,
`chartjs-adapter-date-fns.bundle.min.js`, `chartjs-plugin-zoom.min.js`) with
[`addons/packaging/replace_chartjs.sh`](../../addons/packaging/replace_chartjs.sh): the trio
registers a superset of the above with the date-fns adapter, and the script rewrites the
template, fixtures and tests that name the bundle.

### ES6 modules (`js/app/`)

Modules are plain ES6 (no bundler). Load order matters: `state.js` first, `main.js` last.
The server minifies them, like `style.css`, at startup (`web.minify_assets`); set it to `false`
to read the original sources in browser developer tools.

`main.js` `init()` first arranges the cards in the stored order (see [Card order](#card-order)),
then loads translations, restores the URL state and layout, then waits for one
painted frame (`afterNextPaint()`) before `initCharts()` builds every Chart.js instance: the build
holds the main thread, and the static page should be on screen first. It runs once per page
load: `/api/config` arrives later with the server's Graph Bounds defaults, and those, like a
bound applied from a card's menu, change the Y-axis ceilings in place (`applyChartBounds()`,
for every chart created with a `graphBound` in its Y-axis options).

| Module | Responsibility |
|--------|----------------|
| `state.js` | Shared app state, color palette, global Chart.js config (**load first**) |
| `prefs.js` | Import-free preference storage that survives blocked or full `localStorage` |
| `history-navigation.js` | Shareable URL state, bounded Back/Forward stack, and deterministic interval clamping |
| `main.js` | Entry point; wires event listeners and static chart-card actions, starts auth + WebSocket (**load last**) |
| `api.js` | URL helpers that prepend `window.KULA_BASE_PATH` (base-path support) |
| `auth.js` | Auth status check, config fetch, login/logout |
| `websocket.js` | WebSocket connect and reconnect; queues live samples while a history load replaces the view |
| `charts-init.js` | Chart.js instance creation; full dashboard init; app-chart teardown |
| `charts-data.js` | Sample ingestion, chart updates, zoom sync, gap insertion, device selectors; drains the live queue once a history load lands (`drainLiveQueue`) |
| `container-apps.js` | Multi-series container charts (one per metric type) with the shared application filter |
| `disk-identity.js` | Persistent disk-ID keys, member lookup, and unstable-name labelling for device selectors |
| `chart-controller.js` | Chart registry, animation-frame batching, viewport culling, plot-width budgets |
| `chart-ui.js` | Coalesces chart visibility, subtitles, and container controls during history replay |
| `chart-interactions.js` | Pointer/keyboard pan and zoom, shared crosshair, and gesture-safe tooltips |
| `chart-envelope.js` | Compact extrema arrays plus Min–Max and missing-interval chart bands |
| `chart-accessibility.js` | Canvas names/summaries, keyboard point cursor, bounded semantic tables, and complete chart CSV |
| `chart-card-actions.js` | Expand and save-as-image buttons plus hover/touch pause, shared by static, dynamic and split chart cards |
| `chart-image.js` | Import-free PNG export: composites a chart canvas with its card title, details, host and time range |
| `card-order.js` | Import-free card ordering rules: default order, stored-order resolution, saved-order merge, drop position |
| `card-layout.js` | The one placement authority: registers template cards, places every dynamic card, arranges sections or Focus Mode's combined grid |
| `rearrange.js` | Rearrange mode: grips, pointer drag, keyboard moves and announcements; stores the order through `card-layout.js` |
| `format.js` | Metric and Local/UTC formatting, datetime conversion, and bucket-tooltip semantics |
| `history-request.js` | Abortable, generation-safe latest-history-request controller; live-refresh cadence and rolling tail requests |
| `history-data.js` | Canonical history items, aggregation validity, view signatures, tail merging, missing-observation gaps, and Focus Mode API sections |
| `date-range-calendar.js` | Single-month range calendar; retention disables days, never times |
| `clock-picker.js` | Clock dial and hour/minute/second wheels for one end of the custom range, in the UI language's clock |
| `gauges.js` | Bar gauges, sparkline backgrounds, live gauge updates |
| `controls.js` | Pause/resume, layout toggle, time-range selection, history fetch |
| `focus-mode.js` | Select/persist a subset of chart cards; `card-layout.js` combines them into one grid |
| `tv-mode.js` | Import-free TV mode over Focus Mode: viewport-filling grid, idle cursor, wake lock, full screen |
| `section-utils.js` | Resolves section titles to their charts grid, including the Applications header wrapper |
| `split.js` | Per-device/interface graph splitting |
| `header.js` | Header bar + chart subtitle updates |
| `system-info.js` | System Info page: inventory request lifecycle and in-place live-value patching |
| `settings.js` | Dark/light theme and the Customization menu (appearance, accessibility, chart data/tooltips/Min–Max bands, Local/UTC) with persisted preferences |
| `alerts.js` | Alert evaluation (clock sync, low entropy, overload) + dropdown |
| `i18n.js` | Fetches translations from `/api/i18n` and applies to the DOM |
| `ollama.js` | AI assistant panel; SSE streaming from `/api/ollama/chat` |

Modules keep preferences in `localStorage` only through `prefs.js`. When site data is blocked,
every access to `localStorage` throws, and a full quota makes writes throw. `readPref()` and
`readJsonPref()` then return nothing or their fallback, a corrupt JSON value included, and
`writePref()` keeps the value in memory, so the page still sees what it last wrote. The dashboard
starts either way and its preferences last until reload. `TestDashboardStorageGoesThroughPrefs`
rejects direct `localStorage` use in any other module except the import-free `tv-mode.js`,
which guards its own calls, and the live-dashboard browser test loads the page with storage
blocked.

### History and rendering

WebSocket samples enter `charts-data.js`; preset, custom, zoom and reconnect requests share
one abortable `HistoryRequestController`. Only its current generation can replace history.
Failures retain the successful view and expose a failed status. Reconnection refreshes the
whole selected preset. Custom/zoomed intervals stay frozen while live gauges and status update.
WebSocket close code 1008 signals session expiry or revocation: the dashboard cancels pending
history, stops reconnecting, clears authenticated readings and returns to login. A refused
handshake (a 401 once the session expired while the page slept) reaches the page only as a
close before open, so after one the dashboard asks `/api/auth/status` and does the same when
the session is gone; otherwise (server down, connection limit) it keeps backing off (×1.5 per
attempt up to 30 s, each wait drawn between half and all of it). Successful
login opens a fresh connection and reloads the selected preset or custom interval.

Every buffer observation uses the canonical `ts`, `data`, optional `min`/`max`, `dur`, and
bucket-metadata shape. `available_aggregations` controls the selector (a choice the response
cannot serve falls back to Avg but is kept in `suspendedAggregation` and restored by the first
response that supports it); per-bucket
`extrema_profile` and shared `extrema_profiles` restrict individual scalar fields. Older
API responses fall back to their strict `valid_aggregations` list. Unknown profiles expose
no extrema. Representative data supplies identities and chart structure; point ingestion
selects the validated field value or a null gap. Split and application charts use the same
path. Bands, detailed tooltips, chart notices and CSV retain the same availability; mixed
legacy/current histories do not hide valid newer extrema. CSV adds source columns where
needed and leaves unavailable numeric values blank.
Partial profiles copy only allowlisted own scalar properties into objects without prototypes;
inherited properties and nested wildcards cannot expose additional fields. Callers normalize
unavailable readings to null before point ingestion, which also discards their extrema so
Min/Max, bands and CSV cannot restore a sentinel as a measurement. Filesystem tooltip byte
counts come from the selected aggregation block in both single and split charts.
Response provenance is shared between observations and indexed by timestamp for tooltips.
Tier `0` is not synonymous with raw output: `downsampled` responses still refetch on zoom and
retain valid Min/Max controls, including below three hours. The share URL preserves an allowed
non-default aggregation regardless of window length. Local raw-buffer zooms preserve partial
coverage and gap markers instead of unconditionally promoting a view to complete.

Requests use 300–5,000 observations based on the widest visible plot. There is room for one
explicit gap marker between adjacent observations (at most 10,000 buffer items). Gap markers
never evict observations. Short rolling windows append live data while it fits the selected
budget; long windows refresh the entire range at display resolution. Appending also requires
the loaded response to be a native view: tier 0, output resolution equal to its source
resolution, and no Min/Max offered (`historyViewAcceptsLiveSamples`). The point budget alone
cannot predict this, because the server counts epoch-aligned buckets: an unaligned 5-minute
window spans 301 one-second buckets, so a 300-point request returns 2s buckets. Such bucketed
views refresh once per output step instead of receiving raw live samples, which carry no
extrema and would otherwise render as gaps in Min/Max. Their axes remain at the
last successful snapshot until replacement, including during network failures. The refresh
cadence and snapshot time appear in the resolution tooltip. Still-selected history is never
removed to make room for a stream of raw points.

A rolling view refreshed in buckets — a reduced tier-0 view, or any view built from tier 1 or
2 — refreshes by its **tail**, not the whole window (`fetchRollingTail` in `charts-data.js`).
`rollingTailRequest` re-fetches the last three buckets — the trailing one is still filling, and
the one before it can still lack the part of a source record that ends in the next bucket —
from a bucket boundary, asking for exactly one point per bucket. Every finer server step would
need more buckets (consecutive steps differ by at least 1.5×), so the server answers at the same
step and epoch alignment and computes each bucket exactly as a full refresh would. The request
names the view's tier (`rollingTailSource`, the `tier` hint of `/api/history`), or a few minutes
of a tier-1 view would be answered from raw data. Native tier-1/2 records end on bucket
boundaries (rollup windows are epoch-aligned), so a native view's request starts one
millisecond past the boundary, leaving out the record that ends exactly there. `mergeHistoryTail` replaces the buffered buckets
from that boundary on and re-derives the gap markers; the merged buffer is then rendered like a
full response, and its coverage combines the previous left edge with the tail's right edge
(`mergedTailCoverage`). The left edge counts as retained once the observed data reaches it, so
the next complete tail clears a partial verdict a lagging one left behind.
The tail is used only when its `historyViewSignature` (tier, step, source resolution,
aggregations, extrema profiles) matches the snapshot it extends, with the same point budget
and Focus Mode sections; anything else, a failed or empty tail, or a snapshot older than five
minutes, falls back to a full refresh. On a 30-minute view that is a few kilobytes per step
instead of megabytes. `TestHistoryResponsesMatchDashboardContract` checks against a real store
that the tail is answered at the view's step and that the merged buffer equals a fresh full
refresh.
The live cadence estimate uses a bounded median of observed live timestamp differences, so
supported slower collectors are not permanently treated as one-second sources. Reconnects
reset that estimate; historical responses never supply its timestamp baseline.

`chart-controller.js` batches updates in animation frames and uses IntersectionObserver to
defer layout and drawing for off-screen charts. It reads all visibility rectangles before
drawing. Cursor changes use `render()`; metric/scale changes use `update('none')`. Grid/list
changes resize existing instances without refetching history, preserving legend selections
and data. `format.js` reuses a bounded formatter cache; tick measurement samples eight labels.

During each flush, dirty off-screen charts still call the vendored Chart.js private
`_updateHiddenIndices()` method. It drains the `_dataChanges` mutation records and adjusts
hidden-point indices after inserts and trims without running layout or drawing. Charts remain
dirty so their deferred update runs when they enter the viewport. Clearing the records directly
would lose point-visibility bookkeeping. The x-axis tick callback in `charts-init.js` also reads
the time scale's private `_unit` (`this._unit`) to pick seconds, minutes or dates for its labels;
without it every label falls back to hours and minutes. `TestChartBundle` asserts that both
members exist, in the vendored bundle and in the upstream files `replace_chartjs.sh` installs.
Revalidate these private contracts with the browser regressions whenever Chart.js is upgraded.

History loads, local zooms, and aggregation changes ingest their samples in a synchronous
`batchChartUI` call. It commits only the last presentation update for each key, retaining
every observation and extrema pair. New chart structures are still created when discovered.
Device selectors replay only their affected charts, preserving other datasets and shared gap
metadata. Chart points use numeric epoch milliseconds and finite numeric values or explicit
`null` gaps, so Chart.js can run with `parsing: false`.

Time series are straight and unfilled. Trusted extrema remain in flat companion arrays, with
one principal-series band by default (CPU uses total usage). Additional bands are optional in
Customization. Min/Max selection and Data/CSV can inspect extrema from every series. Derived
sums across devices remain line-only because independent extrema may occur at different times.
`history-data.js` retains exact missing-observation bounds and inserts null line breaks. The
chart plugin paints those bounds as neutral, text-free background bands; `join_metrics` is the
explicit operator override for joining lines across them without hiding the bands.
Sensor series keep stable identity across reorder/disappearance, and new sensors receive
null backfill. Missing applications/devices append nulls to retained series, preserving outages.

Local/UTC selection lives in Customization and changes presentation without changing instants.
Tooltips show exact times, bucket bounds, selected source/native resolution, output resolution,
source-record contributor count, coverage and aggregation validity.

### Chart interaction and accessibility

The gesture layer provides selection zoom, Shift+drag/touch pan, two-finger pinch and Ctrl+wheel
zoom. Back/Forward use a bounded dashboard navigation stack. Live returns to the last preset;
Zoom out doubles an exact interval up to 31 days. Completed gestures create one request and
history entry; the first continuous gesture frame invalidates pending history requests before
synchronizing scales. Zooming within the loaded view retains at least twelve observations
(excluding gap markers), with a twelve-source-interval floor shared by Chart.js and the gesture
handler. Panning into a new range still depends on that range's available history.
Refetching finer history can lower that floor for the next gesture.
Every gesture uses the same 31-day/future-edge clamp. Changing language recomputes the active
preset/custom label rather than reapplying the initial five-minute placeholder. Twelve-hour
tick labels reserve explicit skip padding. The custom picker derives precision from the
collection interval and refreshes selectable tier-header retention ranges when opened;
outside pointer/click actions dismiss its unapplied draft.

Split-card Graph Bounds menus use the shared document outside-click handler in `main.js`.
Opening buttons and interactions inside the menu stop click propagation; outside clicks that
reach the document, including unrelated icon clicks, dismiss the menus. `split.js` keeps menu
listeners on the card rather than adding a document listener for every rebuild. Split teardown
destroys the registered chart instances and removes their cards, allowing the discarded DOM and
its listeners to be collected.

Each canvas has a visible-heading accessible name, summary and keyboard controls. Enter pins a
point, arrows step through pinned observations, Home/End selects endpoints and Escape clears
it. Unpinned arrows pan and +/- zoom. Pointer hover shares a crosshair without announcements or
changing the header layout; only an explicit pin displays a timestamp there. Drag completion is
filtered from click-to-pin handling. Keyboard changes use a card-local status region. History
transitions use one global region.

Data controls are off by default. `chart-accessibility.js` receives a preference callback from
chart creation, creates a Data button only after opt-in, and creates table/export DOM only on
first use. Opt-out removes those nodes without removing canvas accessibility. The table preview
shows up to 50 timestamps; CSV includes all represented observations in the selected viewport,
with representative values and trusted extrema. Spreadsheet formula strings are escaped.

Each card's 📷 button (`addSaveImageButton`, placed before the expand button) exports a PNG
entirely in the browser. `chart-image.js` copies the rendered Chart.js canvas 1:1 at its current
device-pixel ratio. Legend, bands, gap shading and a pinned crosshair are already in the
canvas. It paints the page and translucent card backgrounds under it, so the image is opaque in
either theme. Above the chart go the card title, subtitle and visible device-selector values;
below it go the hostname and the viewport range (`formatFullTimestamp`, honoring Local/UTC).
Footer labels wrap to two rows rather than truncating the hostname. Layout mirrors for RTL
languages with explicit left/right alignment, because canvas `direction` is not honored
everywhere. The module has no imports, so `history_frontend_test.mjs` can load it directly.

### Card order

`card-layout.js` is the only code that inserts or moves dashboard cards. `initCardLayout()`
registers the template's gauges and chart cards in template order. Every card created later
enters through `placeCard(card, { section, family, anchor, rank })`: split cards (`split.js`),
battery/UPS cards and application cards (`charts-data.js`, `container-apps.js`). Never append
a card yourself. `arrangeCards()` computes each section's order (gauges, system, thermals,
applications) from the cards present and the stored order `kula_card_order`, using the pure
rules in `card-order.js`, and moves only the cards that are out of place. The layout therefore
depends only on which cards exist, the stored order and the mode. Focus Mode, telemetry arriving
late and re-created cards cannot shift anything. Position means DOM position. No card carries a
persistent inline CSS `order`, so Tab order, TV mode and the screen agree. Expanding a card
still uses temporary orders, which `chart-card-actions.js` restores.

The default order is:

1. template cards, in template order;
2. split cards right after the card they replace (`anchor`), in device order;
3. families without an anchor: batteries/UPSes by name, then the application groups in
   `APP_GROUPS` order. Each group's cards go by rank; custom groups go by name.

A card the stored order does not know goes after the nearest card of its family, or its anchor,
that precedes it by default. Failing that, it goes before the nearest card of its family that
follows it, and otherwise at the end of its section. A second battery joins the first. A new
interface follows its siblings or its parent card. Postgres replication joins the Postgres
cards. When saving, `mergeSavedOrder()` keeps the ids of cards that are absent at that moment
after their old predecessors, up to 300 ids per section. Absent cards include an application
that is down or a split view that is off.

Focus Mode marks the selected cards `focus-visible` and calls `arrangeCards()`. In Focus Mode
it concatenates the system, thermals and applications sections into `#charts-grid`, each in
its own order. Leaving Focus Mode puts every card back in its section. Functions registered
with `registerCardDecorator()` run on every placed card: they give new cards their Focus
classes and, while rearranging, their grips.

Rearrange mode (`rearrange.js`) is opened from the Customization menu. It appends its bar to
the sticky top bar and gives each card a grip. The rest of the card becomes `inert`: chart
gestures, tooltips and header buttons stop, and Tab moves between grips. Cards move only within
their section, inside Focus Mode's combined grid too.

- **Pointer.** A drag starts after 4 px: anywhere on a card for mouse and pen, on the grip for
  touch. The grip has `touch-action: none`, so the rest of the page still scrolls. The pointer
  is captured on the section container, because moving the captured card would release the
  capture. Once per animation frame, the card's node moves to `dropIndex()` among the visible
  siblings, and the page scrolls when the pointer is near the viewport edges. `pointerup` and
  `pointercancel` drop and store. <kbd>Esc</kbd> restores the stored order instead.
- **Keyboard.** Arrow keys move a card one place among the visible cards of its section;
  Left/Right are mirrored in RTL. Home and End move it to the first or last place.

Every move is stored at once and announced in a polite live region inside the bar. **Cancel**
restores the order the mode was entered with, and **Reset order** removes `kula_card_order`.
Entering the mode collapses expanded cards and clears the chart search. Focus Mode is
unavailable until the mode ends.

Focus Mode selects API metric sections through `history-data.js`; leaving or changing
Focus Mode refreshes the interval to populate newly visible cards. The default dashboard asks
for all sections. Opening the dashboard does not start a background storage-coverage scan.

TV mode (`tv-mode.js`) is a presentation of an applied Focus Mode selection. It never
changes the selection or the history sections. The Focus bar's TV checkbox is read when
**Done** applies a selection. `kula_focus_tv` persists the choice, and `main.js` restores it
after `applyStoredFocusMode()`. `html.tv-mode` hides the chrome in CSS and turns the dashboard
into a viewport-height flex column with `#charts-grid` taking the remaining space. Focus Mode
has already moved every selected card into that grid, in the stored card order. `tvGridShape()` picks columns and rows
that fit the widest 2:1 chart, among the shapes whose rows all fit at the stylesheet's
`--tv-row-min` (10rem, so it grows with the text size). A short display therefore gets fewer,
wider rows rather than a grid that scrolls out of view. The result goes into
`--tv-columns`/`--tv-rows`, and `.tv-fill` marks the last card to span a partial row. A
`MutationObserver` on the grid's children and a
`ResizeObserver` on the grid recompute the grid shape in one animation frame. Cards come and
go with telemetry, and the gauge row and text size change the available height; the grid does
not rely on `applyStoredFocusMode()` re-running. Chart.js resizes canvases through its own
observers. `syncPauseState()` ignores hover pause while `isTvModeActive()`, because a wall
display's cursor rests on a card. Manual and zoom pauses still apply. The module holds a
screen wake lock and requests it again when the page becomes visible. The static
`#tv-controls` toolbar in `index.html` gets its translations from the normal `data-i18n` pass.
The module has no imports, so `history_frontend_test.mjs` loads `tvGridShape()` directly.

Browser regression commands and performance fixtures are described in [Testing](12-testing.md).

### Base-path awareness

The server injects `window.KULA_BASE_PATH` into the HTML template; `api.js` prepends it to every
request so the SPA works unchanged when mounted under a reverse-proxy prefix.

### Theme before first paint

The server renders `global.default_theme` into `<body data-default-theme>`, and a nonce'd inline
script at the top of `<body>` resolves the theme the way `settings.js` does (stored `kula_theme`,
else that default, else `auto` via `prefers-color-scheme`) and adds `light-mode` before anything
paints. `state.js` starts from the same default, so the first `applyTheme()` agrees with it
instead of waiting for `/api/config`. Change the resolution in both places together.

### Adding a chart for a new metric type

When you add an application metric type, the frontend side is: add the group to `APP_GROUPS` in
`card-order.js`, create the chart dynamically on first data (`if (s.apps?.foo) { ... }`) with
`createAppChartCard(..., 'foo', rank)`, which places the card through `card-layout.js`, and
register the card ID in `charts-init.js`'s `destroyAppCharts()` for cleanup. See
[Adding a Metric Type](14-adding-metrics.md#11-frontend-charts).

### System Info page

`system-info.js` renders the hardware inventory fetched from `GET /api/system-info` (see
[Web Server & API](07-web-server-api.md#get-api-system-info)). It has its own request lifecycle:
chart history, pause state, and the WebSocket buffer never supply its values. A poll that only
changes a temperature or a byte count patches the few live widgets in place instead of rebuilding
the page, so scroll position, text selection, hover state, and the mount search survive every
refresh. The Storage, Network, Connected devices and Sensors tabs are gated on
`show_system_details` from `kula-config-ready`: while it is false only the System tab is built,
the storage and network summary cards are omitted, and a `#system-info/<section>` hash naming a
hidden section falls back to the System tab.

### Easter egg

A Space Invaders clone (`game.html`/`game.js`, Press Start 2P font) is reachable from a header
button when `global.easter_egg` is true. It is a classic (non-module) script and is not
translated. It reads and writes `localStorage` through its own `readStored()`/`writeStored()`,
so with storage blocked it runs and keeps settings and the high score until reload.

- **Graphics settings** live in the `gfx` object. Every option is a string enum listed in
  `GFX_OPTIONS`, and `GFX_PRESETS` holds low/medium/high. Low reproduces the original mobile look
  and medium the original desktop look, so the defaults (low on touch devices, medium elsewhere)
  render as before. `loadGfx()` validates each stored field of `kula_invaders_gfx` on its own, so
  a corrupt or stale value falls back to the default for that field only. `applyGfx()` rebuilds
  whatever depends on a setting (starfield, particle cap, canvas backing store) and syncs
  `aria-pressed` on the panel's buttons.
- **Sharp resolution** sizes the canvas backing store to the displayed size in device pixels
  (`renderScale`, capped at `MAX_RENDER_SCALE`). `draw()` sets that scale as the transform each
  frame, so game logic keeps using 800×600 units. `shadowBlur` ignores the transform, which is
  why glow goes through `setGlow()`, which scales the blur.
- **Settings and game state.** Opening the panel pauses a running game, and `settingsOpen` keeps
  the level-up timer from resuming play underneath it. The `settings-open` body class hides the
  other overlays so changes preview on the canvas. While the panel is open the keydown handler
  ignores game keys, so Tab, Enter and Space operate the panel's buttons.
- **Restart** (`restartGame()`) works from the pause menu or the game over screen. A paused run
  goes through `saveHighScore()` but never reaches `game_score_url`. Only `gameOver()` submits.
- **Fixed-rate game loop.** `loop()` draws once per animation frame but runs `update()` in fixed
  60 Hz ticks (`TICK_MS`), carrying the leftover time forward. A slow frame therefore catches up
  instead of putting the game in slow motion, and 120/144 Hz displays do not speed it up. A 10%
  slack absorbs frame-timestamp jitter so a 60 Hz display gets exactly one tick per frame. Stalls
  over `MAX_CATCHUP_MS` (a hidden tab) are dropped. Game timers (cooldowns, invincibility, enemy
  fire) count ticks, so they keep their meaning.
- **Scanlines** are a pattern fill painted onto the canvas by `drawScanlines()`, not a CSS overlay.
  A full-page overlay above the constantly redrawn canvas made the browser re-composite the page
  every frame. That dropped software-rendered Firefox from 60 to about 14 fps, and removing only
  the overlay's flicker animation still left it near 35.
- **FPS counter** counts animation frames over one-second windows and also shows the longest
  frame in the window, so a single stall is visible even when the average stays at 60.

---

## Terminal UI (TUI)

Package: [`internal/tui`](../../internal/tui/), built with **Bubble Tea** + **Lipgloss**.

| File | Role |
|------|------|
| [`tui.go`](../../internal/tui/tui.go) | Bubble Tea model: rolling metric rings, tab navigation, refresh loop, `Options` |
| [`view.go`](../../internal/tui/view.go) | The seven live views (Overview, CPU, Memory, Network, Storage, Processes, GPU) with gauges and responsive layout |
| [`history.go`](../../internal/tui/history.go) | The History view: chart definitions, window/range state, background loads |
| [`chart.go`](../../internal/tui/chart.go) | Braille line charts with envelopes, axes and time ticks; block area charts for live trends |
| [`styles.go`](../../internal/tui/styles.go) | Adaptive foreground palette (no painted background) shared by every view |

`tui.RunHeadless(collector, tui.Options{...})` drives it. The live views sample through the
TUI's own collector. The History view queries a `tui.HistorySource`, which `cmd/kula` provides
by opening the configured storage with `storage.OpenReadOnly` (see
[Storage Engine](05-storage-engine.md#read-only-access)).

History loads run as Bubble Tea commands, so a slow read never blocks input. At most one load
is in flight: a change while one runs is queued, and a result for a superseded window is
discarded. At the live edge the view reloads on an interval scaled to the range, only while it
is the active tab and sampling is not paused. It requests about one bucket per braille dot
column (`historyPoints`) and reloads when the plot width changes. All charts share a y-label
width (`historyLabelWidth`) so their time axes line up. The store picks the tier and step, and
the view reads the step back from `HistoryResult.Resolution`. Buckets are placed at their
midpoints, and a spacing wider than about two steps breaks the line (`historyData.layout`).

Layout rules the views rely on:

- Three text tones, each with one job (documented in `styles.go`): text for values and names,
  muted for labels, units, details and hints, faint for decoration only (rules, separators, bar
  tracks). Faint is too low in contrast for information, especially on light backgrounds.
- `valueStyle` / `isZeroValue` mute values that read as a single zero, in tables and metric
  grids alike.
- `View` renders at `frameWidth()` (the terminal width capped at `maxFrameWidth`) and centres
  the result. `contentWidth()` derives from the same cap, so scrolling maths in `Update` agrees
  with what is drawn.
- `newGaugeLayout` sizes bars from the width alone, so every bar in a view shares its columns.
  A detail that does not fit beside the bar wraps under it instead of being truncated.
- `metricGrid` sizes each grid column from its own items: values right-align down a column,
  and labels shorten only when the column's longest label and value cannot share a cell.
- `renderTemperature` draws a gauge on a 0–100 °C track using `newGaugeLayout`, so thermals
  share the percentage gauges' columns.
- `trendRows`, `overviewTrendRows` and `historyChartRows` size charts from the content height. An
  80×24 terminal keeps one-line sparklines, and the Overview must fit it without scrolling
  (`TestOverviewFitsStandardTerminalWithoutScrolling`).

See the user-facing [Terminal UI](../user/06-tui.md) page.

Next: [Internationalization](11-i18n.md).
