# Web Dashboard

The dashboard is a single-page application embedded directly in the Kula binary. It is built
on Chart.js with custom SVG gauges, connects over WebSocket for live updates, and falls back
to the history REST API for longer time ranges.

Open it at `http://localhost:27960` (or your configured address). If `web.ui` is `false`, the
dashboard is disabled and only `/metrics` and `/health` remain.

## Layout

- **Header** — hostname, current system and hardware info, Kula version, theme toggle,
  language selector, and (when enabled) the AI assistant 🤖 button and the Space Invaders
  easter-egg button.
- **Gauges** — at-a-glance circular gauges for the headline metrics (CPU, memory, etc.).
- **Chart cards** — one card per metric group: CPU, Load, Memory, Swap, Network, Disk I/O,
  Filesystems, Thermals, GPU, Processes, Battery, and any enabled Applications / Custom
  metrics.

## Features

### System Info

Choose **System Info** (📡) in the header to open the dedicated current-inventory page. The
at-a-glance view puts the server identity, uptime, CPU, memory, main storage, and primary network
connection first. Lower-level identifiers, counters, mount options, and device tables are grouped
under **Technical details** so they remain available without overwhelming the main view. Use
**Back to dashboard** or the browser Back button to return to the charts.

The page refreshes every five seconds while open and visible, including when charts are paused or
viewing a historical range. Leaving it or hiding the browser tab stops requests. **Refresh now**
retries immediately; server discovery is shared across clients for up to five seconds. The
`#system-info` URL fragment makes the page navigable with browser Back and Forward.

- **Overview**: OS, kernel, architecture, system/motherboard identity, BIOS/firmware,
  uptime, clock synchronization, processes, and current resource usage.
- **CPU**: processor model/features, physical cores, sockets, logical CPUs, NUMA nodes,
  shared caches, frequency policies, temperatures, and kernel-reported mitigations.
- **Memory**: RAM/swap usage, memory accounting, SMBIOS module slots, manufacturer,
  part/serial numbers, capacity, type and speed, plus EDAC details/error counters when exposed.
- **Storage**: physical and virtual drives, partitions, capacity, model/serial/WWID,
  sector sizes, backing devices, I/O rates, busy percentage, and mounted filesystem usage.
- **Network**: all visible interfaces, including virtual interfaces and loopback; addresses,
  MAC, driver, MTU, state, link speed, separate receive/transmit rates and utilization,
  and cumulative byte/error/drop counters.
- **Devices**: current GPU metrics and PCI/USB inventory with device IDs and available drivers.
- **Sensors & power**: readable temperatures, fans, voltages, current, power, humidity,
  and battery/UPS/mains attributes.

The page reads `/proc` and `/sys` directly without requiring external utilities. Available
details depend on the machine, drivers, permissions, and container namespaces. Missing
readings appear as `—`; memory module records often require additional read permissions.
SMBIOS and EDAC describe their own views of modules and are labeled by source.

Disk and network rates need two readings and reset after a long idle gap or device replacement.
Network utilization requires a known link speed. I/O busy measures active time, not remaining
drive bandwidth. Filesystem usage is shown per mount; shared volumes can appear on multiple
backing drives, so their capacities should not be summed. Remote/FUSE mount usage is available
when supplied by the configured collector. The displayed inventory and metric collection
timestamps make stale readings visible, and request failures are reported on the page.

Inventory is held in memory and never written to history. Disable both the page and its API
with `global.show_system_info: false`.

### Time range & history

By default the dashboard streams live 1-second data over WebSocket. Selecting a longer time
window switches to the history API, which serves downsampled data from the appropriate
storage tier (1-minute or 5-minute aggregates for older data).

You can pick a **preset window** (1m … 30d) or a **custom range** with explicit from/to
timestamps, up to 31 days. The calendar button opens labeled From/To fields in the displayed
time zone, starting with the current chart range. In the calendar, click the first day and then
the last day to select a range, including both days. Month/year controls allow jumping to older
dates; the From/To fields allow precise time adjustments. Quick choices fill in the last hour, last
24 hours, today, or yesterday. Check the duration and choose **Apply range**; **Cancel** or
Escape discards your edits. Invalid or oversized ranges show a message before a request is sent.

The dashboard samples history to the chart width and keeps time-axis labels horizontal.
Resolution, source tier, and partial coverage appear below the controls; tooltips show exact
bucket details.

Chart tooltips show the timestamp and metric values by default and disappear during chart
gestures. Enable **Show detailed chart tooltips** in Customization to add bucket boundaries,
source, contributing records, coverage, and aggregation details.

Long live windows refresh the whole selected interval at display resolution. This preserves
older history as new samples arrive. The range stays at its last successful snapshot during a
refresh; hover the resolution for the refresh interval and last update. Failed refreshes are
reported while the previous chart remains visible. Gauges and status continue to update live.

- **Live** returns to the most recently selected live preset.
- **Back / Forward** moves through the dashboard's viewport history.
- **Zoom out** doubles an exact interval, up to the 31-day limit.

### Shareable URLs

The selected view is reflected in the URL:

- `range=<seconds>` selects a supported preset, such as `?range=86400` for 24 hours.
- `from=<ISO>&to=<ISO>` selects an exact custom interval and takes precedence over `range`.
- `agg=avg|min|max` requests an aggregation when the response advertises it as valid.

### Interactive zoom

Drag-select to zoom, Shift+drag to pan, or Ctrl+wheel to zoom. Touchscreens support horizontal
one-finger pan and two-finger pinch. A focused canvas supports arrow-key pan and +/- zoom.
Enter pins the closest observation; while pinned, Left/Right steps through points, Home/End
jumps to the visible endpoints, and Escape clears the cursor. Double-click or choose Live to
return to the most recent preset.

Zoom-in stops at twelve observations. Its minimum duration follows the source sampling
interval (twelve seconds for one-second samples); missing intervals do not count as points.
If a view already contains fewer than twelve observations, it cannot be zoomed in further.

Exact custom and zoomed intervals, including device selectors and split charts, stay frozen.
Live samples still update gauges, headings, subtitles and alerts. Hovering shares a crosshair
across visible charts; clicking pins its time.
Transient hover positions stay inside the charts and do not alter the history-information row.

Choose Local or UTC in **Customization → Charts** to set the display zone for chart ticks,
tooltips, pinned times, the header clock, and custom date/time inputs. The selected instants do
not change when switching zones. Historical tooltips report bucket bounds, source/output
resolution, contributing source-record counts, coverage and valid aggregation semantics.

The date picker uses whole seconds unless `collection.interval` is below one second.
Dates outside every retained tier range are disabled, and partial first/last days are clamped
to retained times. Typed dates are validated too. The ranges come from tier headers: outage
days inside a retained range may remain selectable and are shown as gaps in the charts.
Click anywhere outside the picker, or press Escape, to close it without applying the draft.

### Chart accessibility and data tables

Every time-series canvas has a name, accessible summary and keyboard controls. History
transitions and keyboard point changes are announced; pointer hover remains silent.

Data controls are off by default. Enable **Show chart data controls** in **Customization →
Charts**, then choose **Data** on a card. The table is created on first use and shows the latest
50 visible timestamps, including representative values and trusted extrema. **Download CSV**
exports all observations represented in that chart's viewport, including rows omitted from the
preview. Text that could be interpreted as a spreadsheet formula is neutralized. Disabling the
preference removes the controls and tables while retaining basic chart accessibility.

### Focus mode

Hide everything except the charts you care about. Useful when investigating a specific
subsystem. While Focus Mode is active, history requests omit unrelated high-cardinality metric
sections and refetch them automatically when the selection changes or Focus Mode is closed.

### Y-axis bounds

Charts can auto-scale to data peaks, or you can impose fixed maxima. For CPU temperature, disk
temperature, and network, the bound mode (`off` / `on` / `auto`) is set in
[configuration](04-configuration.md#web) under `web.graphs`. In `auto` mode Kula tries to
detect a hardware limit (CPU TjMax, disk thermal max, NIC link speed).

### Per-device selectors & split view

Network, Disk I/O, Disk space, Disk temperature, and GPU charts can show all devices on one
chart or be **split** into one chart per device/interface. Toggle this per-chart with the
split (⊟) button, or set defaults under `web.graphs.split`.

### Layout toggle

Switch between a **grid** layout and a **stacked list** layout. Existing charts, history and
legend selections are preserved; switching does not refetch history.

### Aggregation selector

The dashboard shows aggregation choices only when `/api/history` declares them valid across
every displayed metric in the response. New or recomputed policy-reducer buckets offer Avg,
Min, and Max; unbucketed raw points and legacy rollups offer representative data only. Min and
Max are per-series extrema within each bucket, so values across different lines need not have
occurred at the same instant. Avg is duration-weighted for sampled gauges and rates; monotonic
counters and fixed capacity/metadata values retain their latest observation. Existing
`web.default_aggregation` settings remain the preferred operation whenever the response
supports it; fresh installs default to Avg.

For validated aggregate history, the principal series has a restrained Min–Max band. Enable
**Show Min–Max bands for all series** in Customization for additional bands. Every series keeps
its extrema for Min/Max selection and Data/CSV, regardless of band visibility.
The dashboard never draws a band from legacy or otherwise untrusted envelopes. Selected
single-device and split charts pair extrema by stable device identity; an all-device derived
sum stays line-only because component extrema may have occurred at different times.

### Gap handling

By default, gaps between measurements (e.g. after a restart) break the plotted lines and shade
the missing interval with a neutral grey band. The band has no label or annotation. Set
`web.join_metrics: true` to connect lines across gaps; the shaded interval remains visible.

### Alerts

The dashboard raises in-UI alerts for:

- **Clock not synchronized** — system time isn't synced to an NTP source.
- **Low entropy** — the kernel entropy pool is depleted.
- **System overload** — sustained high load.

### Theme & language

- Light / dark / auto theme (set the default with `global.default_theme`).
- 26 UI languages; selector can be hidden/forced via
  `web.lang`.
- Some new history/navigation labels currently fall back to English while translations catch up.

### AI assistant

When Ollama is enabled, a 🤖 button opens a local AI analysis panel. See
[AI Assistant](10-ai-assistant.md).

## REST API & WebSocket

The dashboard is driven by a small JSON API. If you want to build your own client or scripts,
the endpoints are documented for developers in [Web Server & API](../dev/07-web-server-api.md).
The headline endpoints:

| Endpoint | Purpose |
|----------|---------|
| `GET /api/current` | The latest sample |
| `GET /api/system-info` | Current system/hardware inventory and utilization |
| `GET /api/history?from=&to=&points=&sections=` | Time-range history (downsampled; optional section selection) |
| `GET /api/config` | UI configuration (theme, langs, graph bounds, custom metrics) |
| `GET /ws` | WebSocket live stream |
| `GET /metrics` | Prometheus exposition (if enabled) |
| `GET /health`, `GET /status` | Liveness |

Next: [Terminal UI](06-tui.md).
