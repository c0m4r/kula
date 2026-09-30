# Terminal UI (TUI)

Kula includes a terminal dashboard for when you're on a server over SSH and don't want to open
a browser. It's built with [Bubble Tea](https://github.com/charmbracelet/bubbletea) and
[Lipgloss](https://github.com/charmbracelet/lipgloss).

## Launch

```bash
./kula tui
```

The TUI runs **independently** of the `serve` daemon: it spins up its own collector and
samples the system directly, so the live views work without `kula serve`. The **History** view
charts what `kula serve` has recorded. It opens the storage tier files **read-only**, never
writes, migrates or resizes them, and keeps up with a service that is running at the same time.

## Tabs

The TUI presents eight views; navigate between them with the keyboard:

| Tab | Shows |
|-----|-------|
| **Overview** | A condensed summary of all key metrics and a health line |
| **CPU** | Per-aspect CPU usage and load averages |
| **Memory** | Memory and swap breakdown |
| **Network** | Per-interface throughput and TCP/socket stats |
| **Storage** | Per-device I/O, disk temperatures, and filesystem usage |
| **Processes** | Running / sleeping / blocked / zombie counts and threads |
| **GPU** | GPU load, power, VRAM, temperature |
| **History** | Charts of stored history, from 5 minutes up to 30 days |

Each view uses bar gauges, trend charts and a responsive layout that adapts to your terminal
size; terminals narrower than 32 columns or shorter than 8 rows show a "Terminal too small"
notice instead. Colors are adaptive foregrounds with no painted background, so the TUI stays
legible in both light and dark terminal themes.

On a standard 80×24 terminal the live trends are one-line sparklines. Taller terminals grow them
into multi-row charts labelled with their scale, and the Overview tiles grow too. Receive and
transmit trends share one scale, so a small upload never looks as busy as a large download. The
Processes view adds an **Other** row for tasks outside the four listed states (mostly idle
kernel threads), so the rows add up to the total.

A few conventions keep the screens quick to read:

- Values are in the brightest tone, labels and details a step quieter; the faintest tone is only
  used for rules and separators.
- Values that are zero (`0`, `0.00`, `0 B/s`) are dimmed, so the numbers that are moving stand
  out in tables and metric grids.
- Numbers stacked in a column are right-aligned, and every bar in a view (including
  temperatures, drawn on a 0–100 °C track) starts and ends in the same columns.
- History chart titles give the latest reading (`now`, or `last` for a past window) with the
  window's average and peak; multi-line charts list each line's latest value in the legend.
- On terminals wider than 136 columns the layout stays 136 columns wide and is centred, so
  labels stay close to their values.

## History

The History view charts the samples `kula serve` stores: CPU usage, load average, memory and
swap, network throughput, disk I/O and utilization, TCP connections, processes, CPU temperature,
and GPU load. A chart with no data in the window (no swap, no GPU) is left out.

- **Ranges.** `-` picks a smaller window and `+` a bigger one, stepping through 5m, 15m, 1h,
  3h, 6h, 12h, 24h, 3d, 7d and 30d; the range bar shows `-` at the short end and `+` at the long
  end. The view opens on the last hour.
- **Panning.** `[` and `]` move the window by half its width. Panning back stops at the oldest
  retained sample; panning forward to the present, or pressing `n`, resumes following.
- **Following.** At the live edge the view reloads on its own (every couple of seconds for short
  ranges, up to once a minute for long ones) while it is visible and sampling is not paused.
  `r` reloads immediately.
- **Resolution.** Charts use the finest storage tier that covers the window, reduced to about
  one bucket per braille dot column. The line under the range shows the window, the bucket size
  and the source resolution, and flags a window that starts before the data does.
- **Peaks.** Where the storage kept each bucket's minimum and maximum, single-metric charts draw
  that range as a shaded band behind the average line, and the `max` beside the title is the
  true peak rather than the highest average.
- **Gaps.** Periods without data, such as the service being stopped, appear as breaks in the
  line rather than being bridged.

The History view reads the directory set in `storage.directory`. It needs permission to read the
tier files, which the packaged service creates as its own user with mode `0600`. Run the TUI as
that user or as root with the service's configuration, for example:

```bash
sudo kula --config /etc/kula/config.yaml tui
```

With the default `storage.directory` of `/var/lib/kula`, a user who cannot write there falls back
to `~/.kula`, which holds only history recorded by a `kula serve` that user ran. The view says
which directory it reads and why it is empty or unreadable. Storage it could not open is tried
again every few seconds while the view is shown, so it starts charting once the files become
readable, without a restart. The tier files do not record their resolutions, so the view takes
them, and the tier it reads for a window, from the configuration it was started with. When the
files' sizes or number differ from that configuration, it says so above the charts: pass the
service's `--config`.

## Keys

| Key | Action |
|-----|--------|
| `Tab` / `→` / `l` | Next view |
| `Shift+Tab` / `←` / `h` | Previous view |
| `1` … `8` | Jump directly to a view |
| `↑` / `k`, `↓` / `j` | Scroll one line |
| `PgUp` / `Ctrl+U`, `PgDn` / `Ctrl+D` | Scroll one page |
| `g` / `Home`, `G` / `End` | Jump to top / bottom |
| `Space` | Pause or resume sampling (and History following) |
| `r` | Sample immediately; on the History view, reload stored history |
| `-` / `+` | History: smaller / bigger range (`_` and `=` also work) |
| `[` / `]` | History: pan back / forward (`,` `.` and `<` `>` also work) |
| `n` | History: return to now |
| `?` | Keyboard help overlay (`Esc` closes it) |
| `q` / `Q` / `Ctrl+C` | Quit |

The header reports live state — `STARTING`, `LIVE`, `STALE` or `PAUSED` — next to the current
time, and the footer shows the keys for the current view and a scroll indicator when the view
overflows.

## Refresh rate

The refresh interval is set separately from the collection interval:

```yaml
tui:
  refresh_rate: 1s
```

Values below 100 ms are capped to protect terminal responsiveness. The live trends keep
roughly two minutes of history regardless of the interval (bounded to 30–240 samples); use the
History view for anything older.

## Quitting

Press `q`, `Q` or `Ctrl+C` to exit. Application monitoring (Postgres, MySQL, nginx, Apache2,
containers) is started for the TUI session as well, so those collectors initialize on launch.

## System info

The header shows the hostname, and the Overview view closes with an
OS · kernel · architecture line, unless `global.show_system_info` is `false`, in which case
both are hidden.

Next: [Authentication](07-authentication.md).
