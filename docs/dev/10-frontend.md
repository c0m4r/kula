# Frontend (SPA & TUI)

Kula has two user interfaces: the embedded **web SPA** and the **terminal UI**. Both are part of
the single binary.

---

## Web dashboard (SPA)

Source: [`internal/web/static/`](../../internal/web/static/), embedded via `//go:embed`.

It is a dependency-light, framework-free single-page app built on **Chart.js** (bundled,
vendored under `js/chartjs/`) with custom SVG/bar gauges. It connects over WebSocket for live
data and falls back to the history REST API for longer ranges.

### Asset layout

```
static/
├── index.html              # main dashboard template (CSP nonce + SRI injected by server)
├── style.css               # dashboard styles
├── kula.svg / favicon.ico  # branding
├── game.html / game.css / game.js   # Space Invaders easter egg
├── fonts/
│   ├── Inter/              # UI font (OFL-1.1)
│   └── Press_Start_2P/     # game font (OFL-1.1)
└── js/
    ├── chartjs/            # vendored Chart.js + zoom + date-fns adapter (min.js)
    └── app/               # Kula's own ES6 modules
```

### ES6 modules (`js/app/`)

Modules are plain ES6 (no bundler). Load order matters: `state.js` first, `main.js` last.

| Module | Responsibility |
|--------|----------------|
| `state.js` | Shared app state, color palette, global Chart.js config (**load first**) |
| `history-navigation.js` | Shareable URL state, bounded Back/Forward stack, and deterministic interval clamping |
| `main.js` | Entry point; wires event listeners and static chart-card actions, starts auth + WebSocket (**load last**) |
| `api.js` | URL helpers that prepend `window.KULA_BASE_PATH` (base-path support) |
| `auth.js` | Auth status check, config fetch, login/logout |
| `websocket.js` | WebSocket connect, reconnect, live-queue drain |
| `charts-init.js` | Chart.js instance creation; full dashboard init; app-chart teardown |
| `charts-data.js` | Sample ingestion, chart updates, zoom sync, gap insertion, device selectors |
| `container-apps.js` | Multi-series container charts (one per metric type) with the shared application filter |
| `disk-identity.js` | Persistent disk-ID keys, member lookup, and unstable-name labelling for device selectors |
| `chart-controller.js` | Chart registry, animation-frame batching, viewport culling, plot-width budgets |
| `chart-ui.js` | Coalesces chart visibility, subtitles, and container controls during history replay |
| `chart-interactions.js` | Pointer/keyboard pan and zoom, shared crosshair, and gesture-safe tooltips |
| `chart-envelope.js` | Compact extrema arrays plus Min–Max and missing-interval chart bands |
| `chart-accessibility.js` | Canvas names/summaries, keyboard point cursor, bounded semantic tables, and complete chart CSV |
| `chart-card-actions.js` | Expand-button and hover/touch pause interactions shared by static and dynamic chart cards |
| `format.js` | Metric and Local/UTC formatting, datetime conversion, and bucket-tooltip semantics |
| `history-request.js` | Abortable, generation-safe latest-history-request controller |
| `history-data.js` | Canonical history items, aggregation validity, missing-observation gaps, and Focus Mode API sections |
| `date-range-calendar.js` | Tier-availability calendar for exact historical intervals |
| `gauges.js` | Bar gauges, sparkline backgrounds, live gauge updates |
| `controls.js` | Pause/resume, layout toggle, time-range selection, history fetch |
| `focus-mode.js` | Select/persist a subset of chart cards |
| `section-utils.js` | Resolves section titles to their charts grid, including the Applications header wrapper |
| `split.js` | Per-device/interface graph splitting |
| `header.js` | Header bar + chart subtitle updates |
| `system-info.js` | System Info page: inventory request lifecycle and in-place live-value patching |
| `settings.js` | Dark/light theme and the Customization menu (appearance, accessibility, chart data/tooltips/Min–Max bands, Local/UTC) with persisted preferences |
| `alerts.js` | Alert evaluation (clock sync, low entropy, overload) + dropdown |
| `i18n.js` | Fetches translations from `/api/i18n` and applies to the DOM |
| `ollama.js` | AI assistant panel; SSE streaming from `/api/ollama/chat` |

### History and rendering

WebSocket samples enter `charts-data.js`; preset, custom, zoom and reconnect requests share
one abortable `HistoryRequestController`. Only its current generation can replace history.
Failures retain the successful view and expose a failed status. Reconnection refreshes the
whole selected preset. Custom/zoomed intervals stay frozen while live gauges and status update.

Every buffer observation uses the canonical `ts`, `data`, optional `min`/`max`, `dur`, and
bucket-metadata shape. Rendering selects only operations allowed by `valid_aggregations`.
Response provenance is shared between observations and indexed by timestamp for tooltips.
Tier `0` is not synonymous with raw output: `downsampled` responses still refetch on zoom and
retain valid Min/Max controls, including below three hours. The share URL preserves an allowed
non-default aggregation regardless of window length. Local raw-buffer zooms preserve partial
coverage and gap markers instead of unconditionally promoting a view to complete.

Requests use 300–5,000 observations based on the widest visible plot. There is room for one
explicit gap marker between adjacent observations (at most 10,000 buffer items). Gap markers
never evict observations. Short rolling windows append live data while it fits the selected
budget; long windows refresh the entire range at display resolution. Their axes remain at the
last successful snapshot until replacement, including during network failures. The refresh
cadence and snapshot time appear in the resolution tooltip. Still-selected history is never
removed to make room for a stream of raw points.
The live cadence estimate uses a bounded median of observed live timestamp differences, so
supported slower collectors are not permanently treated as one-second sources. Reconnects
reset that estimate; historical responses never supply its timestamp baseline.

`chart-controller.js` batches updates in animation frames and defers off-screen charts using
IntersectionObserver. It reads all visibility rectangles before drawing. Cursor changes use
`render()`; metric/scale changes use `update('none')`. Grid/list changes resize existing
instances without refetching history, preserving legend selections and data. `format.js`
reuses a bounded formatter cache; tick measurement samples eight labels.

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

Focus Mode selects API metric sections through `history-data.js`; leaving or changing
Focus Mode refreshes the interval to populate newly visible cards. The default dashboard asks
for all sections. Opening the dashboard does not start a background storage-coverage scan.

Browser regression commands and performance fixtures are described in [Testing](12-testing.md).

### Base-path awareness

The server injects `window.KULA_BASE_PATH` into the HTML template; `api.js` prepends it to every
request so the SPA works unchanged when mounted under a reverse-proxy prefix.

### Adding a chart for a new metric type

When you add an application metric type, the frontend side is: define an `APP_ORDER_*` constant
in `charts-data.js`, create the chart dynamically on first data (`if (s.apps?.foo) { ... }`), and
register the card ID in `charts-init.js`'s `destroyAppCharts()` for cleanup. See
[Adding a Metric Type](14-adding-metrics.md#11-frontend-charts).

### System Info page

`system-info.js` renders the hardware inventory fetched from `GET /api/system-info` (see
[Web Server & API](07-web-server-api.md#get-api-system-info)). It has its own request lifecycle:
chart history, pause state, and the WebSocket buffer never supply its values. A poll that only
changes a temperature or a byte count patches the few live widgets in place instead of rebuilding
the page, so scroll position, text selection, hover state, and the mount search survive every
refresh.

### Easter egg

A Space Invaders clone (`game.html`/`game.js`, Press Start 2P font) is reachable from a header
button when `global.easter_egg` is true.

---

## Terminal UI (TUI)

Package: [`internal/tui`](../../internal/tui/), built with **Bubble Tea** + **Lipgloss**.

| File | Role |
|------|------|
| [`tui.go`](../../internal/tui/tui.go) | Bubble Tea model: rolling metric rings, tab navigation, refresh loop |
| [`view.go`](../../internal/tui/view.go) | The 7 tab views (Overview, CPU, Memory, Network, Storage, Processes, GPU) with progress bars and responsive layout |
| [`styles.go`](../../internal/tui/styles.go) | Dark purple/slate theme with style caching for performance |

`tui.RunHeadless(collector, refreshRate, osName, kernel, arch, version, showSystemInfo)` drives
it. The TUI runs its own collector and does **not** read the storage tiers — it samples live.
See the user-facing [Terminal UI](../user/06-tui.md) page.

Next: [Internationalization](11-i18n.md).
