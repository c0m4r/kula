# Terminal UI (TUI)

Kula includes a terminal dashboard for when you're on a server over SSH and don't want to open
a browser. It's built with [Bubble Tea](https://github.com/charmbracelet/bubbletea) and
[Lipgloss](https://github.com/charmbracelet/lipgloss).

## Launch

```bash
./kula tui
```

The TUI runs **independently** of the `serve` daemon — it spins up its own collector and
samples the system directly. You don't need `kula serve` running to use it. (It does not read
from or write to the storage tier files.)

## Tabs

The TUI presents seven views; navigate between them with the keyboard:

| Tab | Shows |
|-----|-------|
| **Overview** | A condensed summary of all key metrics |
| **CPU** | Per-aspect CPU usage and load averages |
| **Memory** | Memory and swap breakdown |
| **Network** | Per-interface throughput and TCP/socket stats |
| **Storage** | Per-device I/O, disk temperatures, and filesystem usage |
| **Processes** | Running / sleeping / blocked / zombie counts and threads |
| **GPU** | GPU load, power, VRAM, temperature |

Each view uses bar gauges, trend sparklines and a responsive layout that adapts to your terminal
size; terminals narrower than 32 columns or shorter than 8 rows show a "Terminal too small"
notice instead. Colors are adaptive foregrounds with no painted background, so the TUI stays
legible in both light and dark terminal themes.

## Keys

| Key | Action |
|-----|--------|
| `Tab` / `→` / `l` | Next view |
| `Shift+Tab` / `←` / `h` | Previous view |
| `1` … `7` | Jump directly to a view |
| `↑` / `k`, `↓` / `j` | Scroll one line |
| `PgUp` / `Ctrl+U`, `PgDn` / `Ctrl+D` | Scroll one page |
| `g` / `Home`, `G` / `End` | Jump to top / bottom |
| `Space` | Pause or resume sampling |
| `r` | Sample immediately |
| `?` | Keyboard help overlay (`Esc` closes it) |
| `q` / `Q` / `Ctrl+C` | Quit |

The header reports live state — `STARTING`, `LIVE`, `STALE` or `PAUSED` — next to the current
time, and the footer shows a scroll indicator when the active view overflows.

## Refresh rate

The refresh interval is set separately from the collection interval:

```yaml
tui:
  refresh_rate: 1s
```

Values below 100 ms are capped to protect terminal responsiveness. The trend sparklines keep
roughly two minutes of history regardless of the interval (bounded to 30–240 samples).

## Quitting

Press `q`, `Q` or `Ctrl+C` to exit. Application monitoring (Postgres, MySQL, nginx, Apache2,
containers) is started for the TUI session as well, so those collectors initialize on launch.

## System info

The header shows the hostname, and the Overview view closes with an
OS · kernel · architecture line, unless `global.show_system_info` is `false`, in which case
both are hidden.

Next: [Authentication](07-authentication.md).
