# Custom Metrics

Kula can chart **anything** you can measure. Define chart groups in `config.yaml`, then push
JSON values into a Unix domain socket. Each group becomes a chart card; each metric in the
group becomes a line on that chart.

## 1. Define groups in config

```yaml
applications:
  custom:
    cpu_fans:
      - name: fan1
        unit: RPM
        max: 5000
      - name: fan2
        unit: RPM
        max: 10000
    room_temp:
      - name: ambient
        unit: °C
        max: 50
```

Each metric has:

- **`name`** — the series name (must match the key you send).
- **`unit`** — label shown on the chart (e.g. `RPM`, `°C`, `req/s`).
- **`max`** — Y-axis maximum for the chart.

These definitions are exposed to the frontend via `/api/config`, so the dashboard knows the
units and bounds for each custom chart.

## 2. Send data to the socket

Kula creates a Unix socket at `<storage.directory>/kula.sock`. Send a JSON object whose
`custom` field maps group names to arrays of `{name: value}` objects:

```bash
echo '{"custom":{"cpu_fans":[{"fan1":4423},{"fan2":8512}]}}' \
  | socat - UNIX-CONNECT:/var/lib/kula/kula.sock
```

You can send from any language that can write to a Unix socket. A polling script can push
values on the same interval as Kula's collection loop.

## 3. View

The new chart cards appear under the **Applications** section of the dashboard automatically
once data arrives.

## Example: a polling script

A reference Python example ships at [`scripts/custom_example.py`](../../scripts/custom_example.py).
A separate NVIDIA exporter,
[`scripts/nvidia-exporter.sh`](../../scripts/nvidia-exporter.sh), feeds the built-in GPU charts
by writing `<storage.directory>/nvidia.log`; it does not use the custom-metrics socket.

A minimal shell loop:

```bash
SOCK=/var/lib/kula/kula.sock
while true; do
  rpm1=$(cat /sys/class/hwmon/hwmon0/fan1_input 2>/dev/null || echo 0)
  printf '{"custom":{"cpu_fans":[{"fan1":%s}]}}' "$rpm1" \
    | socat - UNIX-CONNECT:"$SOCK"
  sleep 1
done
```

## Notes

- The socket path follows `storage.directory`; if Kula fell back to `~/.kula`, the socket is
  there instead.
- One process owns the socket. If another Kula process is already serving it, a new one logs
  that the socket is in use and runs without custom metrics instead of taking it over, and
  `kula tui` never opens it, so the running service keeps its clients.
- Group and metric names should be stable — they key the charts. Values may be integers or
  floats. Sending the same name twice in one message keeps only the last value.
- History stores each value as a 32-bit float: about seven significant digits, and integers
  exact only up to 16,777,216 (2²⁴). The live view shows the value as sent. A counter or ID
  past that range comes back rounded; send a rate, or scale it down, instead.
- A value beyond ±3.4 × 10³⁸, the 32-bit float range, is refused and logged once: the metric
  is left out of its message, as if it had no reading. A message whose every value is refused
  is ignored, so the group keeps its last values until it goes stale (see below). Leave a
  metric out of the message when it has no reading, rather than sending a sentinel such as the
  largest double.
- Messages are newline-delimited JSON — one object per line, at most 64 KB per line. Only
  metrics declared for a group in the config are stored; unknown names are ignored. The socket
  is created with `0660` permissions (owner + group), and connections that stay idle for five
  minutes are closed.
- At most 64 connections are served at once. One more is closed as soon as it is accepted, and
  the refusal is logged at most once a minute, so close a connection when its push is done, or
  keep a single one open per producer. When a message is not valid JSON, the log line quotes the
  decoder's error, so any control characters a client sent appear escaped.
- Custom metrics are stored alongside everything else in the tiered ring-buffer, so they have
  history and appear in downsampled views too.
- **Staleness.** A group stops reporting once its producer goes quiet, leaving a gap on the
  chart rather than a frozen line. The window defaults to a few `collection.interval` cycles;
  set `applications.custom_stale_after` (e.g. `10s`) to override. Push roughly on the
  collection interval to keep a feed live.

Next: [AI Assistant](10-ai-assistant.md).
