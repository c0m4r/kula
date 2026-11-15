# Kula 0.22.0 — Storage, codec and history: are values stored and shown correctly?

| | |
|---|---|
| **Commit** | `5cb10ea` (branch `main`) |
| **Version** | `VERSION` = `0.22.0` |
| **Date** | 2026-11-12 |
| **Reviewer** | Claude Opus 5.5. Codec, rollups, history queries and the API first-hand; two parallel reviewers for the dashboard/CSV/TUI and the collectors. Every reviewer claim used here was checked against the code, and the collector reproductions were rerun. |
| **Scope** | The path of a value: collector → codec → rollups → history query → `/api/history` → dashboard, CSV, TUI and `/metrics`. Ring mechanics, crash recovery and clock steps were covered by `Fable_5.2.md` and are not repeated. |
| **Status** | Complete for its scope. Nothing fixed; no repository file changed besides this one. |

**Marks.** *Measured*: I ran it (a scratch test, or this host's live data). *Reproduced*: a reviewer's test that I reran, with the code path read. *Reproduced (reviewer)*: a reviewer's run I did not repeat, with the code path read. *By reading*: code only. The scratch tests are not part of the repository.

---

## 1. Verdict

The storage engine keeps what it is given, faithfully. Rollup means are weighted by real elapsed time and equal the raw mean; the minimum and maximum of every field survive every tier and every re-bucketing; windows sit on the clock, so a minute is the same minute in every view; partial windows keep their true contributing seconds and outages stay holes. I found nothing that corrupts a stored record or moves a value in time.

The errors are at the two ends of the pipeline:

- **Before storage.** Some values are wrong when the collector hands them over. Container disk I/O is doubled on LVM and LUKS hosts; database cache-hit ratios are lifetime ratios; socket counts miss IPv6; a full ext4 filesystem reads 95 %; every start stores one all-zero sample; idle network links are stored as 0; a missing temperature is stored as 0 °C and averaged with real readings. The store cannot tell these from real data, because a stored field has no way to say "unknown" (§5).
- **After storage.** The API rounds integer means (processes blocked 30 % of the time read 0), and the dashboard draws each bucket at its end, hides outages shorter than about two buckets, and joins live samples across a collection stall.

| Severity | Count | Items |
|---|---|---|
| High | 1 | S1 |
| Medium | 8 | S2–S5, A1, P1–P3 |
| Low–Medium | 4 | S6–S8, P4 |
| Low | 18 | S9–S16, A2–A3, P5–P12 |
| Info | 5 | S17, A4–A7 |

Of the fixes in §7, items 1–12 need no codec change except storing MySQL's read-request rate (S3); format changes are confined to the optional item 13.

## 2. The path of a value

1. **Collect.** Rates divide counter deltas by monotonic elapsed time (`collector.go:192-202`), so suspend and clock steps cannot distort them. Percentages are jiffy or byte ratios. Cumulative counters are kept as they are.
2. **Write** (`store.go:261-318`). A record's timestamp is the wall clock and marks the *end* of its interval. Its duration is the spacing from the previous sample, cut to one interval when the gap exceeds two: a gap is missing coverage, not a long observation.
3. **Encode** (`codec.go:388-781`). Rates and percentages as float32, made finite (NaN → 0, ±Inf saturated); bytes and counters as uint64; counts as int32, a few as uint16. One CRC-32 per record.
4. **Roll up** (`store.go:320-512`, `aggregation*.go`). Epoch-aligned 1 min and 5 min windows. A record straddling a window edge is split pro rata; its value and extremes go to both parts. Per field: duration-weighted `mean`, `mean_nonnegative` (drops negative sentinels) or `last`; min and max of everything; dynamic members matched by identity; sparse `MeanStats` carry partial weights and unrounded integer sums to the next tier.
5. **Query** (`store.go:658-1003`, `query_planner.go`, `history_stream.go`). One tier per query: the finest that covers the window within 7,500 source records. Output buckets on epoch-aligned 1/2/5 steps; sources split pro rata; extremes go to every bucket a source touches. Each bucket carries `bucket_start`, `bucket_end`, `coverage` and `sample_count`; `ts` is the end of its last contributing fragment.
6. **Show.** The dashboard plots each bucket at `ts`, draws the average with a min/max band on each chart's principal series, inserts a gap where two points are more than 2.5 steps apart, and appends live samples in native views. The TUI plots at the bucket midpoint.

## 3. Findings

### 3.1 Wrong before it is stored (collectors)

**S1 — High. Container disk I/O is counted once per layer of the device stack.** `containers.go:918-951` adds `rbytes`/`wbytes` over every line of the container's `io.stat`, and the kernel charges a cgroup's I/O to each device it passes through. On this host `user.slice/io.stat` shows byte-identical counters on `259:0` (nvme0n1) and `253:0` (dm-0, LUKS): 69,395,025,920 read, 150,987,853,824 written. A container reading 10 MB/s and writing 20 MB/s is stored as 20 and 40 MB/s; each further layer (LVM on LUKS, say) should add another copy by the same mechanism, and zram swap writes are added too. Default RHEL and Ubuntu server installs use LVM. The host disk collector skips dm/md devices (`disk.go:100-135`), so host and container figures disagree by the depth of the stack. *Measured* (this host's `io.stat`) and *Reproduced* with a fixture. Fix: count each I/O once — at the top of the stack (skip devices that have holders), or at the physical devices as the host collector does — and leave zram out.

**S2 — Medium. A full filesystem reads 95 %.** `disk.go:395-400` computes `UsedPct = (Total − Bfree) / Total`. With ext4's default 5 % root reserve, a filesystem with no space left for users stores 95.00 where `df` shows 100 % (`used / (used + avail)`). This is the value of the Disk Space chart and of `kula_filesystem_used_percent`; there is no built-in disk alert. *Reproduced* (4.096 GB, avail 0 → 95.00). Fix: `Used / (Used + Available)`.

**S3 — Medium. Cache-hit ratios are lifetime ratios.** `postgres.go:539-543` and `mysql.go:481-484` divide totals accumulated since the server started, so on a long-running server the chart is a flat line. PostgreSQL at 1e12 hits / 1e9 reads stays at 99.90 through an hour at a 50 % hit ratio; MySQL moves from 99.90 to 99.86 through an hour at 90 %. *Reproduced.* Fix: compute the ratio per interval, from deltas. Do not then tag it `mean`: a bucket's ratio must be the ratio of sums, and a mean of ratios can read 98 % for a bucket whose true ratio is 50 %. For PostgreSQL the ratio of sums is `mean(blks_hit_ps) / (mean(blks_hit_ps) + mean(blks_read_ps))`, from fields already stored. MySQL needs its read-request rate stored (a new MySQL section version).

**S4 — Medium. `tcp_inuse` and `udp_inuse` count IPv4 sockets only.** `network.go:205-250` reads `/proc/net/sockstat`, never `sockstat6`. A dual-stack listener (the default in Go, Java and Node) accepts IPv4 clients on IPv6 sockets, so on a server the accepted connections are missing. With 100 local clients, `tcp_inuse` rose only by the 100 client-side sockets while `TCP6` took the 100 server-side ones; with remote clients it would not move at all. `curr_estab` counts both families, so the two lines on the Connections chart disagree. *Reproduced* (19 → 119, TCP6 3 → 103). Fix: add `TCP6`/`UDP6` `inuse`.

**S5 — Medium. A host-network container reports the host's traffic.** `containers.go:868-917` sums every interface but `lo` in `/proc/<pid>/net/dev`. A `--network host` container sees the host namespace — `eth0`, `docker0` and every veth — so it reports the host's traffic, several times over. Containers sharing a namespace (pods) each report all of it. *By reading.* Fix: compare `/proc/<pid>/ns/net` with the host's; report nothing for the host namespace and attribute a shared one once.

**S6 — Low–Medium. CPU usage can be stored negative.** In `cpu.go:122-147` each component is clamped at 0 when its counter goes back, but `Usage = 100 − idle%` is not. The kernel documents that iowait can decrease (`Documentation/filesystems/proc.rst`): jiffies first booked as iowait and later as idle push idle above 100 % of the interval. *Reproduced* with a fixture (usage −30.0 where 10 % was busy); not seen live. A negative point also becomes the Min of its windows. Fix: compute usage from the busy fields, clamped to [0, 100].

**S7 — Low–Medium. A missing temperature is stored as 0 °C and averaged in.** CPU, disk and GPU temperatures use 0 for "no reading" (`cpu.go:208-210`; `disk.go:280-283` when the cadence has no current reading; `gpu_sysfs.go:12-15`), but their policy is plain `mean` (`types.go:27`, `:122`, `:170`). A CPU at 60 °C with half its readings missing rolls up to 30 °C; a disk at 45 °C to 22.5 °C. The per-sensor lists are right (a missing sensor leaves the list). The Min block's 0 disappears from the JSON (`omitempty`), so such a bucket shows Avg 30 °C and no Min. A GPU stays in the sample while its VRAM total is known (`gpu.go:180`), so a failed temperature read is averaged in. `history_compatibility.go:5` already calls these "temperature sentinels". *Measured.* Fix: a `mean_positive` policy for temperature fields that leaves values ≤ 0 out of means and extremes. It changes rollups only, not the codec; existing rollups keep their values.

**S8 — Low–Medium. Every start stores one sample of zeros.** The first `Collect` has no baseline (`cpu.go:167-176`; network, disk, self and the application pollers have no previous counters), and `cmd/kula/main.go:281-284` writes and broadcasts it. *Measured* on this host: CPU 0.00 % then 6.40 % a second later, network 0 then 3.12 Mbps, disk 0 then 15 KB/s. Each start puts a 0 into the Min of its minute and five-minute windows for CPU and every rate, and pulls a partial minute's mean down by 1/n. Any change in the number of per-core lines zeroes the aggregate CPU the same way (`cpu.go:159-176`; a CPU taken offline while the system is 50 % busy stores 0.00, *Reproduced*). Fix: collect once and discard before the loop; take the aggregate `cpu` line whatever the per-core count.

**S9 — Low. Network rates are rounded to 0.01 Mbps.** `network.go:184-187` applies `round2` to Mbps, a step of 1,250 B/s per sample. *Measured*, stored mean against true rate:

| True rate | Stored | Error |
|---|---|---|
| steady 300–600 B/s | 0 | −100 % |
| steady 700 B/s | 1,250 B/s | +79 % |
| steady 1–2 KB/s | 1.25–2.5 KB/s | +25 % |
| bursty, mean 200 B/s (over 1 h) | 51 B/s | −75 % |
| bursty, mean 1 KB/s | 912 B/s | −6.5 % |
| 5 KB/s and up | — | under 0.3 % |

Idle, management, VPN-keepalive and IoT links chart as dead, and since the gauge (`formatMbps`) shows Kbps, the 10 Kbps steps are visible. Packets/s and the container byte rates also use `round2`, in units where it is harmless. Fix: drop the rounding; float32 keeps seven digits.

**S10 — Low. Bridges, bonds and VLANs are summed with the ports they carry.** The auto filter (`network.go:63-90`) skips `lo`, `veth*`, `docker*`, `br-*`, `virbr*`, `vnet*`, `tap*` and `tun*`, and keeps `br0`, `vmbr0`, `bond*`, VLANs, `wg*`, `macvlan` and CNI devices. The TUI's totals (`tui/view.go:1308-1314`) add every stored interface, so a bridged or bonded host counts its traffic twice. The dashboard always has an interface selected (`charts-data.js:1707`), so its sum is not reached. *Reproduced* (this host's `v-br0` is monitored and lives under `/sys/devices/virtual/net`). Fix: leave out what is under `/sys/devices/virtual/net`, at least from totals.

**S11 — Low. Only the first CPU package's temperature is read.** `cpu.go:322-324` returns after the first `coretemp`/`k10temp` hwmon device, and those drivers create one per package. Package 0 at 40 °C and package 1 at 90 °C store 40.0. *Reproduced* with a fixture. `DetectTjMax` shares the discovery.

**S12 — Low. One failed energy read doubles an Intel GPU's power.** `gpu_sysfs.go:29-49` keeps the old baseline across a missing `energy1_input`, so the next delta spans two intervals and is divided by one. *Reproduced* (steady 10 W → 20.0 W).

**S13 — Low. Memory "used" matches neither version of `free`.** `cpu.go:418-428` computes `Total − Free − Buffers − Cached`, which counts reclaimable slab as used and shared memory as free. procps-ng 4 shows `Total − Available`, the kernel's own estimate. *Reproduced* on this host: 34.10 % against 36.88 %; the gap grows on hosts with large slab caches. The RAM alert (`alerts.js:51`) uses this value.

**S14 — Low. Partitions are recognised by name.** `disk.go:220-246` misses `nbd`, `rbd`, `pmem` and `zd` partitions, which are then counted as disks next to their parent; `bcache`/`drbd`/zvol stacks count twice. The reliable test is whether `/sys/class/block/<name>/partition` exists. *By reading.*

**S15 — Low. Container rates divide by the time between collection starts.** `containers.go:296-307`; the counters are read after the Docker list call, whose timeout is one interval. A 0.9 s list followed by a 0.05 s one puts a sample off by up to ~85 %. Bucket means cancel out; Max envelopes are inflated. *By reading.*

**S16 — Low. Battery energy follows the voltage.** `psu.go:75-89` converts charge to energy with `voltage_now`, so `energy_wh_full` (a `last` capacity) swings by about ±10 % with the state of charge; `voltage_min_design` gives a stable figure. An unknown capacity is stored as 0 % (`psu.go:52-55`), the same as an empty battery, and the codec's capacity byte cannot hold −1. *By reading.*

**S17 — Info.** `nice` time is in Usage but in no component series, so a niced load shows usage 100 % with user and system at 0 (*Reproduced*). Process states I, T, t, P and X land in no bucket: 138 of 369 processes on this host (`process.go:285-295`). statfs uses `Bsize` where `df` uses `Frsize`; they differ for some FUSE filesystems. Apache's `ReqPerSec`, `BytesPerSec`, `BytesPerReq` and `CPULoad` are mod_status lifetime averages (already deprecated in `/metrics`).

### 3.2 Stored right, served with less than the store knows

**A1 — Medium. Integer means are rounded to whole numbers.** `aggregation.go:282-285` rounds the mean of an integer field into the integer type. The exact sum and weight are kept in `MeanStats` for the next tier, but that field is `json:"-"` and nothing in `internal/web` reads it. *Measured*: processes blocked 30 % of each minute read `blocked: 0` in every 1-minute bucket, both from tier 0 re-bucketed and from tier 1, and 1.4 running processes read 1. This affects every small count: running, blocked and zombie processes, `lavg.running`, the user count, database connections and threads, nginx reading/writing, Apache workers, PSU capacity, Kula's own FDs. A burst covering under half a bucket shows only under Max or in the band, and the TUI's Processes chart has no band. Fix: send the unrounded mean for the fields a bucket rounded; `numericMean` already records exactly those in the bucket's `MeanStats`.

**A2 — Low. Booleans and strings keep the last value, in Min and Max too.** `aggregation_plan.go:94-107` and the default `reduceLastValue`. A MySQL replica whose IO thread was down four minutes out of five shows "running" for the bucket; the same holds for `is_in_recovery`, `clock_synced` and PSU `status`. *By reading.* Fix: document it; better, roll booleans up as the share of time true (a schema change).

**A3 — Low. `/metrics` rates are one-second values.** CPU, disk, TCP and database rates exist in `/metrics` only as `*_per_second` / `*_percent` gauges of the latest sample (`prometheus.go`). A 15–60 s scrape samples one second in fifteen to sixty, so bursts between scrapes are missed, and there is no counter to `rate()` instead. Network, nginx and Apache have `_total` counters. `docs/user/11-prometheus.md` does not mention this. *By reading.*

**A4 — Info. Floats carry their float32 noise into JSON, and the inner `data.ts` can fall outside its bucket.** `getF32` widens float32 to float64, and the JSON encoder writes the float64's shortest form: 12.34 becomes `12.34000015258789` and 0.01 becomes `0.009999999776482582` (*Measured*; about 8 % of a response's bytes). In a re-bucketed raw view `data.ts` is the last source record's own time, e.g. 19:00:10.300 for a bucket ending at 19:00:10.000 (*Measured*). The dashboard uses the outer `ts` (`history-data.js:111,125,288`); other API clients may not.

**A5 — Info.** `lavg.total` (scheduling entities from `/proc/loadavg`) is a saturating uint16 on disk (`codec.go:409`): 70,000 reads back 65,535 (*Measured*). Nothing charts it, and `/metrics` reads the live value.

**A6 — Info. A one-second spike at a window edge is in both windows' Max.** This is documented design (`docs/dev/05-storage-engine.md:74-77`). A sample at hh:01:00.3 covers hh:00:59.3–hh:01:00.3, so a 95 % spike there is the Max of both minute 00 and minute 01, while the means split 0.7 s / 0.3 s exactly. *Measured.* Fine as a choice; it deserves a sentence in the tooltip help.

**A7 — Info. Retention depends on the shape of the host.** Tiers are sized in bytes, records grow with interfaces, disks, filesystems and containers, and a rollup is about 3× a raw record because the Min and Max blocks repeat every name, identity and `last` counter. *Measured* at the default 250/150/50 MB:

| Host | Raw record | Rollup | Tier 0 (1 s) | Tier 1 (1 min) | Tier 2 (5 min) |
|---|---|---|---|---|---|
| small VPS (2 interfaces, 1 disk, 3 filesystems) | 869 B | 2.6 KB | 80 h | 41 d | 68 d |
| desktop (4, 2, 6) | 1.4 KB | 4.1 KB | 50 h | 25 d | 42 d |
| Docker host (33, 2, 10, 30 containers) | 8.1 KB | 24 KB | 8.6 h | 4.3 d | 7.2 d |
| busy Kubernetes node (120, 4, 40, 100 containers) | 27 KB | 82 KB | 2.5 h | 1.3 d | 2.1 d |

Nothing promises a period, and the date picker reads the real retained ranges, so nothing is shown that is not there. But no document helps size the tiers, and the API accepts 31-day windows that a container host never holds.

### 3.3 Shown wrong (dashboard, CSV, TUI)

**P1 — Medium. Buckets are drawn at their end.** `charts-data.js:1266-1275` plots each point at `ts`, which equals `bucket_end` for every full bucket (288 of 289 in a 24 h / 5 min response; the trailing partial bucket sits at its last sample). On a 30-day view with 2 h buckets, a spike at 12:00:03 is drawn at 14:00, and drag-selecting around the drawn peak (13:30–14:30) loads raw data without the spike. The TUI plots at the midpoint (`tui/history.go:794-800`, "so a coarse bucket does not appear shifted right"), so the two views disagree by half a bucket. The tooltip title shows only the end time; the bucket bounds appear only with `chart_tooltip_details`, off by default (`settings.js:104`). *Reproduced (reviewer)* on the real API; code read. Fix: draw buckets as intervals (a step from `bucket_start` to `bucket_end`) or at their midpoint, and snap a zoom selection to the buckets it touches.

**P2 — Medium. Outages shorter than about two buckets are drawn as a continuous line, and the gaps that are drawn are shifted right.** `history-data.js:306-345` marks a gap only when two points are more than 2.5 steps apart. It starts the band one step after the last point and ends it at the next point's `ts`, its bucket end. With aligned buckets a missing bucket is never jitter, but `bucket_start`/`bucket_end` are not used. With a 5 min step, an 8 min outage leaves no gap, and an 11 min outage at 09:01–09:12 is banded at 09:06–09:15. With a 1 h step, a 100 min outage leaves no gap. On a 30-day view a reboot of a few hours leaves only `coverage < 1` in the detailed tooltip. The TUI uses the same rule (`tui/history.go:773-792`). *Reproduced (reviewer)* in node with server-shaped buckets; code read. Fix: a gap is wherever one bucket's data ends before the next bucket's starts; take both from the bucket bounds and draw the band there.

**P3 — Medium. The live view joins samples across a stall.** `pushLiveSample` (`charts-data.js:1898-1912`) appends with no spacing check. A 15 s SIGSTOP of `kula serve` drew a 15.37 s straight segment on a 5-minute native view. Native views never refresh, so the segment stays until the view changes; suspend, a paused VM or a stuck collector do the same. *Reproduced (reviewer)* in Chromium; code read. Fix: apply the history gap rule to live samples.

**P4 — Low–Medium. In UTC mode, day ticks sit at local midnight under UTC dates.** The vendored date adapter's `startOf('day')` uses local `setHours` (`addons/chartjs/date-adapter.js:138-145`), while the labels are formatted in UTC (`format.js:120-145`). In Warsaw "Nov 8" is placed at 2026-11-08T23:00Z; in Kolkata at 18:30Z. On views of three days or more, events read as belonging to the wrong day. *Reproduced (reviewer)*; code read.

**P5 — Low. A selected device that is missing from a bucket is drawn as 0.** Network and packets (`charts-data.js:290-320`) start at `rx = 0` and push it when the selected interface is absent, e.g. a VPN that was down. Disk Space pushes "0.0 % (0 B / 0 B)" for a missing mount (`:397-411`); Disk I/O correctly pushes null. Split charts push nothing for an absent device, and `appendMissing` (`charts-data.js:1046-1056`) skips them, so their lines bridge the absence. A GPU at exactly 0 % and 0 W is drawn as a gap (`:446-451`). *By reading* (reviewer and me).

**P6 — Low. A backwards step of the server clock freezes the live view for the length of the step.** `charts-data.js:1863-1868` drops every sample at or before the last one, and that mark is never reset, not even on reconnect or logout. *By reading.*

**P7 — Low. The TUI ignores extrema profiles.** `tui/history.go:727-733` uses `Min`/`Max` directly, so legacy buckets draw envelopes for fields outside the legacy allowlist, and so do `none` buckets. *By reading.*

**P8 — Low. The TUI's "max" is the largest bucket mean** on charts without an envelope (Network, Disk I/O, busy, Load, Processes), so over 30 days it understates peaks (`tui/history.go:861-878`). "avg" ignores coverage, and "now" is the trailing partial bucket. *By reading.*

**P9 — Low. Units and rounding.** `formatBytesShort` divides by 1024 and writes KB/MB/GB (`format.js:40-47`), while the TUI's `fmtByteRate` uses 1000, so the same disk rate differs by 7 % at G. The network axis writes `toFixed(1)` Mbps (`charts-init.js:321`), so an idle host's ticks all read "0.0 Mbps", and its tooltip has no unit. Packets/s and IOPS round to whole numbers (`:327`, `:349`, `:362`). *By reading.*

**P10 — Low. Fixed y-axis maxima clip without a sign.** This covers Graph Bounds for temperatures and network and a custom metric's `max` (`charts-data.js:999`, `chart-envelope.js:237-238`). Memory, Swap and VRAM also set `y.max` to the latest total during replay (`charts-data.js:275`, `:284`, `:424`), so after swap shrinks from 8 GB to 2 GB, yesterday's 6 GB is cut off. *By reading.*

**P11 — Low. A CSV row does not say which interval or aggregation it holds.** `chart-accessibility.js:304-321` writes one timestamp column (the bucket end, in the UI's language and zone, not ISO) with no bucket start, duration or coverage. Under Min/Max the value column holds the extreme under the plain series label. *By reading.*

**P12 — Low. The custom range picker moves a start in the repeated DST hour back an hour.** `controls.js:374-375`, `format.js:159-169`: in Europe/Warsaw, 01:30Z on Oct 25 shows as 02:30, and Apply parses that as 00:30Z. *Reproduced (reviewer)* in node.

## 4. What Kula does well

**Means that add up.** Weights are wall-clock durations that tile the timeline (a gap is cut to one interval, not stretched over the outage), and a record crossing a window edge is split pro rata, so a tier-1 minute is the duration-weighted mean of its raw seconds. `Fable_5.2.md` checked conservation to 2e-6 under jitter, gaps, restarts and clock steps. Rates are computed over monotonic time, so a duration-weighted mean of rates equals total change over total time.

**True extremes at every zoom level.** The min and max of every field are kept in each rollup and re-reduced exactly, so the Max of a 30-day bucket is the highest one-second value in it, whichever tier answered (*Measured*: a single 95 % second is the Max of its minute). The dashboard draws them by default, as a band around the average.

**Partial windows kept, with their coverage.** A window cut short by a stall, a restart or an outage is stored as a partial rollup with its real contributing seconds, and every history bucket reports `coverage` and `sample_count` (*Measured*: a bucket cut by an outage reports 6 s, coverage 0.6). No window is thrown away for falling short of a fill threshold.

**No data invented across a gap.** A sample after a gap covers one interval; a device, container or application that disappears is simply absent from the reduction (identity union), never a zero; history buckets are not filled where the source did not observe, and nothing is carried forward or interpolated. (The zeros in §3.1 come from the collectors, not from storage.)

**Aligned windows.** Rollup windows and history buckets both sit on the Unix epoch, so a native rollup is exactly one output bucket and a minute reads the same in every view and tier.

**Stable identities.** Disks are tracked by persistent IDs (WWID, EUI, NGUID, or serial+model+namespace), with kernel names only as a fallback, and duplicates are detected so that multipath paths never merge into one series (`disk_identity.go:61-106`). Every dynamic list reduces by identity.

**Exact cascades.** Sparse `MeanStats` carry partial weights and unrounded integer sums to the next tier, so tier 2 is computed from exact sums, not from tier 1's rounded means. A1 is only that the API does not pass them on.

**A store that survives damage.** CRC-32 per record, resync past holes within a bounded budget, a previous ring pass verified record by record, header geometry checks, fuzzed decoders, and a Python decoder kept in parity. A process crash loses nothing, since each record reaches the page cache when written, and a restart replays pending rollups from tier 0. Backups copy a tier consistently without stalling the writer.

**A format that can grow safely.** Positional sections appended behind flag bits and version bytes, finite floats guaranteed, names and lists bounded so that one element cannot fail a record, and `TestCodecPersistsEveryAggregatedNumericField` catching a field without a codec home.

**Bounded query cost.** A fixed 7,500-record source budget independent of the canvas width, streamed JSON, at most two concurrent queries.

## 5. Further improvements

Beyond the findings, six changes would let the stored history say more than it can today. None is a defect; they are listed from most to least useful.

1. **An explicit "unknown".** A field has no value that means "not measured": NaN becomes 0 on encode, and collectors fall back to 0 for a missing temperature, a first sample, a counter reset or an absent capacity. S7, S8 and part of S16 are this one gap. The cheap fix is per-field rollup policy (S7). The thorough one lets float32 fields keep NaN on disk, leaves it out of means and extremes, and writes it as `null` in JSON, so a chart shows a gap wherever nothing was measured.
2. **Raw counters next to rates.** Network interfaces, nginx and Apache keep their cumulative counters; CPU, disk, TCP and database values exist only as rates. Stored counters make totals over any window exact, let rates be recomputed at any resolution, and would give `/metrics` counters to `rate()` instead of one-second gauges (A3). Each needs a new section version.
3. **Coverage on the chart.** Every bucket already carries `coverage` and `sample_count`, but the dashboard shows them only in the detailed tooltip, off by default. Marking partial buckets on the chart itself would make restarts and short outages visible at every zoom level, including the ones P2 hides.
4. **Leaner rollups.** A rollup is about 3× a raw record because the Min and Max blocks repeat every name, identity and `last` counter (A7). Writing those once, in the Data block, would let the same tier size hold considerably more time. This costs retention, not correctness, and needs a codec change.
5. **Retention and durability stated in time.** Tiers are sized in bytes, and nothing tells an operator how long that is on their host (A7) or how much a power loss can take: with no fsync per write, the kernel's writeback window, about half a minute with default settings. Documenting both is the minimum; reporting each tier's expected retention at startup, or accepting retention in days and deriving the size, would make sizing a non-task.
6. **Splicing tiers within one query.** A query reads one tier, so a view built from tier 1 or 2 ends up to two resolutions (2 min or 10 min) behind the live edge. Filling the tail from a finer tier would close it; at those zoom levels it is a few pixels.

## 6. Checked and sound

- **Codec.** Every numeric field with an `agg` policy round-trips. float32 keeps seven digits for rates and percentages (98,765.4321 Mbps → 98,765.43), uint64 is exact for bytes and counters, ±Inf saturates and NaN becomes 0, and names and lists are bounded. Custom-metric precision is documented (`docs/user/09-custom-metrics.md:80-83`), and out-of-range values are refused, not stored. NaN never reaches storage from the database collectors, since both hit ratios guard a zero denominator.
- **Rollup policies fit the fields.** Every cumulative counter is `last`, every rate and gauge `mean`, capacities `last`, and the −1 sentinels `mean_nonnegative`. Fixed-total percentages have mean(pct) = pct(mean). CPU % and disk utilisation have time-proportional denominators, so their weighted means are ratios of sums. A straddling sample's mean splits exactly (*Measured*: 0.7 s / 0.3 s). Rounded integer means stay within [min, max].
- **Queries.** Re-bucketing raw data and reading tier 1 give the same minutes (*Measured*: identical Data/Min/Max in the integer and spike tests). Downsampled raw views carry trusted min/max computed from the raw seconds, an outage leaves no bucket, and coverage and `sample_count` are right for partial buckets.
- **Collection timing.** Rates use the monotonic gap between collection starts, and the reads follow it by 0.1–0.4 ms (network, disk) and ~4 ms (self, GPU) (*Reproduced*). Suspend is excluded by both the clock and the counters. Resets and wraps give 0 for one tick, never a spike. A failed read drops the baseline for network, disk, TCP, containers and both databases. A sample after a 30 s stall carries the 30 s average rate but is weighted as one interval, placed at its end, and the rest of the stall is a hole (`store.go:277-288`), so bucket means are not biased by stalls; a burst inside the stall is averaged into that one sample.
- **CPU.** guest and guest_nice are left out of the total, so they are not double counted. Usage includes nice, iowait, irq, softirq and steal, the common 1 − idle definition; leaving iowait out is the other convention.
- **Disk and network units.** 512-byte sectors; utilisation is io_ticks over elapsed time, clamped at 100 (iostat's %util); dm, md, loop, zram, ram, sr, fd and partitions are excluded in auto mode; Mbps is decimal megabits; swap used is Total − Free; container memory is `memory.current − inactive_file` and container CPU is % of one core, as in `docker stats`.
- **Dashboard.** The join between native history and live samples has no duplicate, backward point or step (Chromium). A server restart mid-view is repaired with a gap within 8 s. Share URLs carry ISO UTC instants, and tooltips name the zone (CET or CEST). Skipped spring-forward times are corrected. Each series takes its own validated extreme, never a sum or ratio of extremes, and invalid extrema become null with a notice. Sentinels (`user_count < 0`, MySQL lag −1) are hidden, and absent optional sections get explicit nulls. CSV leaves unavailable cells blank and escapes formula characters.

## 7. Recommendations

In order of value for effort.

1. **S1** — count container I/O once per device stack.
2. **S8, S6** — collect once and discard before the loop; compute CPU usage from the busy fields, clamped; take the aggregate CPU line whatever the per-core count.
3. **S9** — drop `round2` on Mbps.
4. **S2** — `used_pct = used / (used + available)`.
5. **S4** — add `sockstat6`.
6. **S7** — a `mean_positive` policy on the temperature fields (rollups only; existing rollups keep their values).
7. **S3** — per-interval hit ratios, rolled up as ratios of sums (MySQL needs its read-request rate stored: a codec change).
8. **S5** — detect host and shared network namespaces.
9. **A1** — send unrounded integer means from the bucket's `MeanStats`.
10. **P1–P3** — draw buckets as intervals and snap zoom to them; find gaps from bucket bounds; check spacing on live samples.
11. **P4, P5** — UTC day ticks; null, not 0, for an absent selected device; let split charts show absences.
12. **Docs** — retention per host shape (A7), the power-loss window, and what the `/metrics` rate gauges are (A3).
13. **Later, if wanted** — the improvements in §5: an on-disk "unknown" for float fields, counters for CPU, disk and TCP, coverage on the chart, leaner Min/Max blocks, retention in time, tier splicing.

## 8. Not tested

A live resolution switch at a tier boundary; a rolling-tail merge across a restart (`TestHistoryResponsesMatchDashboardContract` covers the tail merge itself); NVMe native multipath path devices in `/proc/diskstats`; which kernels refuse hwmon reads on a runtime-suspended AMD GPU (S7); whether iowait still decreases on kernel 7.2 (S6).
