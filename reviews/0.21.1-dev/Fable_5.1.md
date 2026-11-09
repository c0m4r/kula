# Kula 0.21.1 — Comprehensive Review (Safety · Performance · Consistency)

Whole-project audit with emphasis on how data is collected, archived and displayed — in
particular the archival (history) path. TUI excluded by request.

| | |
|---|---|
| **Commit** | `33ca1e9` (branch `main`, clean tree) |
| **Version** | `VERSION` = `0.21.1` |
| **Date** | 2026-10-30 |
| **Reviewer** | Claude Fable 5.1 (storage/archival path and measurements first-hand; web/security, collectors, live frontend and codec-tooling parity via four parallel reviewers, whose High findings were re-verified by reading the code) |
| **`go vet`** | clean on all reviewed packages |
| **`FuzzDecodeSample`** | 480,260 execs / 20 s, no crasher |
| **Codec parity** | `addons/inspect_tier.py` vs Go reader: 7,460 records, ~2.3 M decoded leaves, **0 mismatches** (raw, 1 m, 5 m, wrapped rings, legacy JSON, pre-kind-byte binary) |
| **Measurement host** | AMD Ryzen 5 5600H, 36 h of `gen-mock-data` history, default tiers, live `kula serve` |

No repository file was modified during the review.

---

## 1. Overall status

Mature and unusually well-engineered for its size: 804 commits, storage has more test code
than production code (7.4k vs 5.5k lines), fuzz targets on every risky parser, a schema-driven
aggregation layer enforced by `TestAggregationPoliciesCoverSampleSchema`, a positional codec
with real cross-language parity, a hardened HTTP layer (CSRF + Origin, Argon2id, SRI/CSP
nonces, race-free WebSocket hub, Landlock), and docs accurate down to byte offsets.

The archival design — end-timestamped intervals, duration-weighted means with sparse exact
statistics, per-field extrema envelopes, epoch-aligned output buckets, fail-closed legacy
handling — is **sound and correctly implemented**. Weaker areas:

1. two auth-path holes;
2. two metrics that are silently wrong in every tier;
3. sandbox/collector mismatches that fail silently;
4. performance of mid-range rolling views and linear tier scans, which will hurt on the
   arm64/riscv64 targets the project ships for.

---

## 2. Archival pipeline — validity assessment

### What is right (verified)

- **Rates** are computed per tick by the collector (gauges, `agg:"mean"`); counters are
  `agg:"last"`; the dashboard never derives rates from stored counters. Duration-weighted means
  of rates ≈ total / time. Correct.
- **Bucketing.** `reduceHistoryBuckets` (`internal/storage/query_planner.go:244-339`)
  apportions end-timestamped source intervals across epoch-aligned buckets by fraction, scaling
  `MeanStats` sums/weights with each fragment. Cross-batch merges are associative thanks to the
  sparse sum/weight statistics; integer rounding never compounds. Verified to the second: a
  60 s spike starting at 00:10:30 yields 51.67 % / 48.33 % in the two epoch minutes — the exact
  answer.
- **Extrema.** 1 m envelopes are true 1 s extrema; 5 m envelopes are min/max of envelopes;
  raw-only views correctly expose no extrema (`history_stream.go:84-97`); legacy records are
  gated to an audited scalar allowlist (`history_compatibility.go`). The frontend fails closed
  per bucket (`internal/web/static/js/app/history-data.js:175-213`).
- **Ring buffer.** Tail tracking on real record boundaries across variable-size wraps, header
  self-validation, hole resync after unclean shutdown, 64 MiB record cap, snapshot-expiry
  detection when a writer overtakes a batched reader. Coarse-tier fallback is refused for
  retained-but-empty raw intervals (no invented coverage). Outages produce partial rollups
  instead of smearing pre-outage values forward — observed live when host load dropped ticks
  during the review.
- **Planner.** Fixed 7,500-record source budget independent of display density; coverage
  tolerances (1× / 2× resolution) are reasonable; `points` capped at 5,000 (API) / 7,200
  (store); 31-day range cap.

### Measurements (mock host, default tiers)

Raw record ≈ 1.7 KB, rollup ≈ 6.2 KB ⇒ default tiers retain **~1 d 18 h raw / ~17 d at 1 m /
~28 d at 5 m** (`kula inspect` estimates). On a host this rich the 5 m tier does not quite
cover the 31-day query cap.

Cold server-side query: 5 m window 42 ms, 30 m 111–137 ms, 1 h 172 ms (excluding JSON).

| Window @1000 px | Buckets | Tier / step | JSON | gzip | Wall |
|---|---|---|---|---|---|
| 5 m | 168 | 0 / 1 s | 1.0 MB | 97 KB | 39 ms |
| 15 m | 768 | 0 / 1 s | 5.4 MB | 483 KB | 63 ms |
| **30 m** | 836 | 0 / 2 s | **17.4 MB** | **1.37 MB** | **275 ms** |
| **1 h** | 695 | 0 / 5 s | **14.4 MB** | **1.45 MB** | **273 ms** |
| 3 h – 24 h | 36 – 288 | 2 / 5 m | 0.7 – 6 MB | 95 – 773 KB | 12 – 41 ms |

Each reduced bucket carries three full sample graphs (data/min/max, ~7.6 KB each); the `apps`
section is about half of each. `/api/current` (≈ one WebSocket frame) is 2.5 KB / 1.3 KB gzip.

---

## 3. Findings

Tags: **[web]** HTTP/auth/sandbox · **[collector]** · **[storage]** · **[codec]** ·
**[display]** dashboard · **[frontend]** · **[tooling]**.

### High

**H1 [web] `/api/login` works while auth is disabled, mints real sessions, and they stay valid
after auth is enabled.**
`internal/web/auth.go:267-269` (`if !a.cfg.Enabled { return true }`), `internal/web/server.go:903`
(no `Auth.Enabled` check before `ValidateCredentials`/`CreateSession`); `LoadSessions` /
`ValidateSession` never check the username against config. Reproduced: login with auth off →
cookie → persisted to `sessions.json` → accepted on protected routes after enabling auth, for up
to `session_max_lifetime` (7 d).
*Fix:* 404 from `handleLogin` when auth is disabled; `ValidateCredentials` returns `false` when
disabled; drop sessions whose user is no longer configured on load/validate; document deleting
`sessions.json` when enabling auth.
**✅ Fixed in `37c2d26`.** `handleLogin` answers 404 and `ValidateCredentials` returns `false`
while auth is disabled; `LoadSessions` restores nothing with auth off (so the shutdown save
empties `sessions.json`) and drops sessions whose user is no longer configured — the config is
immutable per process, so sessions created in-process need no re-check. Upgrade note in
`docs/user/07-authentication.md`; `sessions.json` format unchanged.
**Follow-up `f280b64`:** a session minted by an older build for a name that *is* configured
still survived an upgrade-and-enable. Each session now records a fingerprint of its account's
Argon2id hash (`credential` in `sessions.json`, ignored by older versions); a restored
session must match, so old sessions are dropped once and a password change signs the account
out. (`f3b9585` dropped the salt from the fingerprint: the hash is already keyed by it.)

**H2 [web] Argon2id hashing has no global concurrency bound → unauthenticated memory
exhaustion.**
`internal/web/auth.go:266-291` runs `argon2.IDKey` (32 MiB each at defaults; also for unknown
users, by design) with only per-IP limits (15 in-flight per IP, 16,384 IPs before fail-closed).
100 sources × 15 ≈ 48 GiB requested. Confirmed: no semaphore anywhere in the package.
*Fix:* counting semaphore (≈ `NumCPU/4`) around hashing with a short acquire timeout → 429/503.
**✅ Fixed in `3aeeaf8`.** `AuthManager.hashSlots` bounds concurrent Argon2id work to
`GOMAXPROCS/4` clamped to 1–4 (≤ 128 MiB at default params); `handleLogin` waits up to 2 s
(or request cancellation) for a slot, else 503 + `Retry-After: 1`.
**Follow-up `d8f0fe4`:** the bound counted hashes, not bytes, so a raised `argon2.memory`
raised the ceiling with it; slots are now also limited to what fits in 128 MiB (at least one).

**H3 [collector] Disk `util_pct` has not been computed since commit `a5ea457` but is stored,
exported and displayed.**
`internal/collector/types.go:121` is never assigned in `disk.go` (io_ticks parsing was removed).
Consumers: `internal/storage/codec.go:440` (f32 in every sample), `internal/web/prometheus.go:244`
(`kula_disk_utilization_percent` = 0 forever), TUI "busy" alerts that can never fire,
`internal/collector/ai.go:53`, `addons/inspect_tier.py:349`.
*Fix:* restore `Δio_ticks / (elapsed·1000) · 100` from `/proc/diskstats` field 12 under the
existing identity/reset guards, or remove the metric from every consumer and the docs.
**✅ Fixed in `d8dbf29`** (restored). `readDiskStats` captures column 12 again; `collectDisks`
computes the busy % under the same identity/diskseq/rollback guards as the other rates, clamped
to 100. No codec change — `util_pct` was always encoded; records written before the fix keep
their 0.

**H4 [codec] `net.tcp.retrans_ps` is never encoded or decoded — history always reads 0.**
`internal/storage/codec.go:352-358` writes `InErrs`/`OutRsts` only; `types.go:100` carries
`Retrans` (`agg:"mean"`); `charts-data.js:314` charts it. Confirmed on 7,200 mock records: the
live tail shows values, any history load or page reload shows 0 for the whole range.
*Fix (per AGENTS.md):* new preamble flag bit 13, section appended after disk IDs, mirrored in
`addons/inspect_tier.py`. Add a test asserting every `agg`-tagged numeric field survives
encode → decode (the schema test only checks tags, not persistence).
**✅ Fixed in `ffea358`.** `flagHasTCPExt` (bit 13) gates a TCP extension closing each variable
block after disk IDs: `version:u8 = 1` + `retrans_ps:f32` (5 B/block), version-tagged for future
TCP fields (`internal/storage/tcp_ext_codec.go`). Mirrored in `inspect_tier.py`; pre-extension
records still decode (rate 0) and match Python. New `TestCodecPersistsEveryAggregatedNumericField`
gives every `agg`-tagged numeric field a distinct value and round-trips Data/Min/Max — before the
fix it flagged exactly `net.tcp.retrans_ps`, nothing else. Next free bit is now 14.
**Follow-up `9420996` (found while fixing the TCP Low):** the rate was 0 live as well on every
real host. The collector looked for `TCPRetransSegs` in `/proc/net/netstat`, which has no such
counter; the kernel reports `RetransSegs` in the `Tcp:` line of `/proc/net/snmp`, which the
collector already parsed (here: 0 read against 12,319). The "live tail shows values" above was
mock data, which sets the field directly. It now reads `RetransSegs`; history written before
holds 0.

**H5 [display/perf] Rolling 30 m and 1 h views re-fetch the whole window every output step and
rebuild every chart.**
`history-request.js:99-108` returns the output step as the refresh interval once step > 1.05×
the sample interval; `charts-data.js:1798-1876` → `refreshRollingHistory` (`:1968`) →
`replaceHistoryBuffer` (`:1274`, `clearAllChartData()` + full re-render). Measured: 30 m
@1000 px ⇒ 17.4 MB JSON / 1.37 MB gzip every ~1.8 s and 275 ms server wall time per refresh
(~15 % of a Ryzen core per open client; 5–8× that on a Pi-class CPU, where a refresh cannot keep
up with its own cadence). At 1,700 px the 30 m view refreshes every ~1.06 s.
*Fix:* fetch only the tail since the last complete bucket (same step/epoch alignment,
append + trim); slim the bucket payload (envelopes only for charted fields, `omitempty`); round
`to` to the step so the server-side cache can hit.
**✅ Fixed in `6bca6df`** (tail fetch; no API change). Bucketed tier-0 rolling views re-fetch only
their last 3 buckets from a bucket boundary with `points` = bucket count, so the server answers at
the same tier/step/alignment (`rollingTailRequest`, `history-request.js`); `mergeHistoryTail`
splices them into the buffer and re-derives gaps. Falls back to a full refresh on any
`historyViewSignature` mismatch, a changed point budget or Focus sections, a failed/empty tail,
or every 5 min. Measured on 3 h of mock history: 30 m view 7.87 MB / 607 KB gz / 119 ms per
refresh → 44 KB / 4.6 KB gz / 0.6 ms; 1 h view 5.86 MB → 35 KB. The contract test proves against
a real store (records straddling bucket boundaries) that tail buckets are byte-identical to a fresh
full refresh (within a relative 1e-9: since `cbfb3e2` a bucket merged across decode batches can
differ in the last bit) and that the merged buffer equals it; the Chromium test asserts
refreshes are tails.
*Not done:* payload slimming and `to` rounding (the tail already removes the bulk), and the
client still redraws all charts from the merged buffer each step (local CPU, no network).
**Follow-ups:** `051912b` — one lagging tail no longer leaves the view "partial" until the next
full refresh (the left edge is derived from the observed start, `mergedTailCoverage`).
`30b1cae` — views built from tier 1/2 (3 h and longer) tail-refresh too, with a new `tier` hint
on `/api/history`; a 3-hour view on 4 h of mock history: 3.75 MB / 461 KB gz / 47 ms per refresh
→ 71 KB / 12 KB gz / 2.3 ms, tail buckets byte-identical to the full view's.

### Medium — storage & display

**S1 [storage/perf] Every history query scans the tier from the segment start to find `from`.**
`internal/storage/tier.go:467-690`: the skip loop (`:630`, `:669`) peeks/discards every record
through a 1 MB bufio, so the whole 222 MB tier-0 file is read per query (40–170 ms page-cached;
a full-disk read when cold). It dominated a 3-minute-window query (174 ms for 163 records).
*Fix:* sparse in-memory `(offset, timestamp)` index built during the open-time scan and appended
every N writes → O(log n) seek.
**✅ Fixed in `cbfb3e2`.** `internal/storage/tier_index.go`: a seek point about every 64 KiB of a
ring pass holding offset, timestamp and the *prefix maximum* timestamp of its pass; scans start at
the last point whose predecessors all precede `from` (binary search), so results equal a full scan
even across a backwards clock step. Points are confirmed against the file (length + exact ns
timestamp) before use; `Tier.Write` maintains them across wraps. Measured on 36 h of mock history
(223 MB tier 0): every query read 223 MB before, now 0.05–6 MB by window; a 3-minute query
23 ms → 1.4 ms and the H5 tail refresh 21 ms → 0.37 ms page-cached.

**S2 [storage/perf] Startup scans tier 0 twice end-to-end, plus tier 1 once.**
`store.go:156` (`ReadLatest(1)`) then `store.go:168` (`ReadLatest(ratio1)`); `ReadLatest`
(`tier.go:692`) is a full forward walk. ~400–650 MB of reads before serving — tens of seconds on
SD-card storage.
*Fix:* one `ReadLatest(max(1, ratio1))` shared by both, or persist tail offsets.
**✅ Fixed in `1b32a06`** (both). `restoreTailState` reads tier 0's tail once for the latest
cache and the tier-1 buffer; `ReadLatest` walks forward only from late seek points. A clean
`Close` saves each index as `tier_N.idx` (temp + rename, CRC-32), adopted only when its
fingerprint matches the tier header exactly — otherwise one walk per tier rebuilds it. Tier format
unchanged; old versions ignore the sidecar. Startup reads on the same dataset: 460 MB → 0.4 MB
after a clean shutdown, 240 MB after a crash. `FuzzSeekSidecar` covers the parser.

**S1b [storage] (found while fixing S1) A wrap could hide a whole ring pass of history.**
When a pass ended with larger records than the pass before it, the writer wrapped while a few
older-pass records still lay past it; `Tier.Write` kept `oldestOff` on them, so every scan read
those few records, stopped at their sentinel and skipped the pass just finished — up to ~1.75 d
of tier 0 (17 d of tier 1) invisible though still on disk, and the planner, trusting the retained
range, showed it as an outage. Reproduced with varying record sizes (a 23 s hole after 68 writes).
**✅ Fixed in `1e16482`**: a wrap always makes offset 0 the oldest record, dropping the older
tail; regression test `TestWrapWhileStillWrappedKeepsFinishedPass`. Files already in that state
self-heal within one pass, as before.

**S3 [storage/consistency] Identical coarse data is bucketed differently for rolling vs
past/custom windows.**
A record overlapping `to` sets `clipRight` (`history_stream.go:53`), forcing reduction of *all*
records at native step (`:71`) with epoch alignment; a rolling window (nothing beyond `to`)
returns native rollups unchanged. Reproduced on a 3 h dataset:
rolling → bucket `00:10:29–00:11:29 = 100 %`, `downsampled=false`;
past window → `00:11:00–00:12:00 = 48.33 %`, `downsampled=true`.
Both are correct averages of their intervals, but avg-line peaks are attenuated up to 2× in
zoomed views and the UI reports a different source/step for the same data.
*Root cause:* count-based rollup windows (`store.go:266`, `flushRollup` at `:278`) are aligned
to process start, not to the epoch.
*Fix:* flush tier-1/2 windows on wall-clock boundaries (native records then coincide with epoch
buckets), or clip only the boundary record.
**✅ Fixed in `e8c02e6`.** Tiers 1 and 2 roll up epoch-aligned windows; a source record
straddling a boundary is split there (duration and sparse weights apportioned, like a history
fragment), so a rollup ends exactly on its window's end. A window flushes as soon as a part ends
on that end, or when a later window's record arrives (outages). Restart replays the pending tail
through the same windows, clipped at the newest rollup, tier 2 first, so a window a crash left
unrolled is written then. `TestRollingAndPastCoarseViewsAgree` (fails on the old code) and two
contract scenarios pin it. The `downsampled` flag still differs (past window: re-bucketed for
its clipped edge), but the buckets and values agree. Record format unchanged; old rollups keep
their process-aligned windows until they age out. The Chromium suite then caught a side effect:
right after a fresh start the first rollup is a partial minute, and the tier ranker counted it
as a full one, so a 5-minute view was answered with one tier-1 bucket instead of raw samples;
coarse coverage is now ranked from the oldest record's end (`4c3974c`).

**S4 [storage] Backups hold the tier read lock for the whole raw copy, blocking collection.**
`tier.go:823` (`SnapshotTo` under `RLock`), `store.go:714`; `Write` needs the exclusive lock, so
the collection loop (`cmd/kula/main.go:244`) drops ticks → a gap of (250 MB ÷ disk throughput)
in every tier at each backup (sub-second on NVMe, many seconds on SD/HDD). Gzip is correctly
outside the lock; acknowledged in `internal/backup/backup.go:7-10`.
*Fix:* chunked copy with the existing snapshot-expiry check, or `copy_file_range` to a temp file.
**✅ Fixed in `ebae4e4`** (copy, then patch; neither suggested fix). The stall was wider than
stated: `WriteSample` holds the store lock while it waits on tier 0, so queries, `/api/current`
and the live stream waited too. `Tier.SnapshotTo` (`internal/storage/tier_snapshot.go`) now
notes the writer's offset and ring pass under the read lock, copies the file unlocked, then
under the lock copies the header and the ring range written meanwhile (through the ring's end if
it wrapped); `Write` touches nothing else, so the copy equals the file at that moment. A lap
during the copy (tiny rings only) falls back to a locked full copy. Measured on a 249 MB tier 0
(NVMe): the longest `WriteSample` during a backup went from 97–235 ms to 0–2 ms, `QueryLatest`
from 80–216 ms to 0–2 ms. `snapshot_test.go` requires byte equality after appends, wraps (fresh
and wrapped rings) and a lap made between copy and patch, and a `Write` to get through
mid-copy. Not tested: bytes a failed write leaves past `writeOff` (patched up to `writeEnd`).
**Follow-up `f0b93a3`:** with `compress`, the raw intermediate is no longer fsynced before it
is gzipped and deleted; only the `.gz` is synced.

**S5 [storage/design] No per-record checksum.**
With no per-write fsync (`tier.go:800`), a partially flushed record whose length prefix and kind
byte survived decodes "successfully" with zeroed tails. Bounded to the last seconds before a
crash, but undetectable. A CRC32 per record would also harden the read-only TUI path. Low–Medium.
**✅ Fixed in `85b5483`.** Records are now kind `0x03`: the encoded sample, then a CRC-32 (IEEE)
over the length prefix and payload (`internal/storage/record_checksum.go`); `0x02` records stay
readable, unverified, until the ring replaces them. Every reader skips a record failing its
checksum — scans, `ReadLatest`, restart replay (so a torn tail reaches neither the latest-sample
cache nor the 1 m/5 m rollups), the TUI — and resync verifies checksummed candidates whole
(rollups outgrew its 4 KiB cross-check). Mirrored in `inspect_tier.py` (`zlib.crc32`);
`FuzzReadRecord` added. Cost: +4 B per record; about 5 % of a record's decode time with hardware
CRC (amd64/arm64), about 25 % with Go's portable code (riscv64), decoded records only. Older
binaries cannot read `0x03` records (0.22.0 already breaks downgrade for rollups).
*Correction:* since `ffea358` a zeroed tail mostly fails decoding anyway (the TCP extension's
version byte reads 0); the silent cases were a lost middle page and stale bytes from an older
ring pass.

**S5b [storage] (found while fixing S5) Resync could spin forever.** A record with a sane header
running past the active segment's end was accepted by resync, rejected by the scan and resynced
at the same byte again, forever, holding the tier lock (writes and the startup index walk
block). Needs a stale or corrupt header `writeOff`. **✅ Fixed in `838e22c`**
(`TestResyncStopsAtARecordPastTheSegmentEnd`).

### Medium — other subsystems

- **[web/collector] Landlock blocks `/var/run/utmp`** (`internal/collector/system.go:93`; no rule
  in `internal/sandbox/sandbox.go:72-87`). "Logged-in users" therefore becomes a census of
  non-root UIDs computed by opening every `/proc/<pid>/status` every second (7 vs `who`'s 2 on
  the review host; reproduced under Landlock). *Fix:* `ROFiles("/run/utmp", "/var/run/utmp")`;
  make the fallback return "unknown".
  **✅ Fixed in `897d9b4`.** RO rules for both utmp paths and `/run/systemd/sessions`; the count
  falls back to logind sessions (`CLASS=user`, not closing) and is `-1` (unknown) without either —
  the `/proc` census is gone. Unknown is stored as the reserved byte 255 (counts clamped to 254),
  `mean_nonnegative`, omitted from Prometheus, hidden in the footer, `—` in the TUI. Also reads
  `ut_user` (offset 44) instead of `ut_line` (the §3 Nit). **Follow-up `8971a38`:** an
  existing but empty utmp (systemd built without utmp support) pre-empted logind and read 0;
  logind now decides whenever utmp records no login.
- **[web] `https://` collector/Ollama targets cannot verify TLS under the sandbox** — no read
  rule for `/etc/ssl`, `/etc/pki`, `/usr/share/ca-certificates`; Go loads system roots lazily
  after `Enforce` (`sandbox.go:117-146`). Same for Postgres `sslmode=verify-*`. *Fix:* add RO
  rules, or pre-load `x509.SystemCertPool()` before enforcing.
  **✅ Fixed in `72d4640`** (pre-load): `Enforce` loads the system roots first whenever an enabled
  target verifies TLS (`needsSystemRoots`: https nginx/Apache2/Ollama URLs, Postgres
  `verify-ca/full`); RO rules for `~/.postgresql` and the `PGSSL*` paths lib/pq reads. Reproduced
  before the fix: 0 roots, `ca-certificates.crt: permission denied`. **Follow-ups `01b3a55`,
  `c4ba249`:** the `PGSSLCRL` rule was dead (lib/pq refuses that variable) and is gone; an Ollama
  URL without a port is allowed the port the client dials (443/80), not 11434; nginx, Apache2
  and Ollama share one 16-bit port parser.
- **[web] Rootless Podman socket** auto-detected by `containers.go:84` is absent from
  `sandbox.go:153-158` → misleading "add user to docker group" log and silent cgroup fallback.
  *Fix:* share one exported path list.
  **✅ Fixed in `a77f87e`.** `collector.ContainerSocketPaths` is the single list; the sandbox grants
  the socket the collector will pick. Rootless candidates now follow the running user
  (`$XDG_RUNTIME_DIR`, `/run/user/<uid>`, still `/run/user/1000`), and another user's rootless socket
  gets its own log message. Correction: on this kernel the V5 ruleset does **not** mediate
  `connect(2)` on pathname Unix sockets (verified), so the fallback came from the hardcoded uid
  1000 and socket permissions, not from Landlock.
- **[collector] nginx reset guard covers only `accepts`** (`nginx.go:91-96`): `handled` /
  `requests` underflow to ~1.8e19 req/s and are written to history. Reproduced. *Fix:*
  per-counter guard as in `apache2.go:250-255`.
  **✅ Fixed in `8b8980c`.** No rates for a poll where any counter went backwards (they reset
  together); a failed poll drops the baseline so the next rate is not doubled. Apache2 had the
  stale-baseline-after-failed-poll pattern too — **fixed in `b4dacc2`** (and a counter missing
  from the page, `ExtendedStatus Off`, is no baseline).
- **[collector] Postgres partial query failure stores zeros into `prev`**
  (`postgres.go:231-235, 322-342`) → the next good tick emits lifetime counters as per-second
  rates. *Fix:* per-counter validity.
  **✅ Fixed in `45c94bd`.** `valid`/`bgValid` per counter group; a poll failing before its
  counters are recorded drops the baseline. MySQL had the stale-baseline-after-failed-poll
  pattern too — **fixed in `62592c3`**.

**DB1 [collector] (follow-up review) PostgreSQL/MySQL monitoring could exhaust `max_connections`.**
Every query was bounded by a context of one collection interval (1 s). A slower statement was
abandoned client-side (go-sql-driver closes the socket; lib/pq marks the connection bad and sends
a cancel request) while the server kept running it, and the next tick opened a new session beside
it — with a persistently slow or stuck statement (e.g. a hanging `SHOW REPLICA STATUS`), one more
server session per second. Sessions carried no server-side limits, so a vanished Kula host left
them for the OS keepalive (2 h) or MySQL `wait_timeout` (8 h); failed connects were retried every
second; and lib/pq blocks on a half-open socket after its context ends.
**✅ Fixed in `0721fc4`** (`internal/collector/dbsession.go`): one pinned `sql.Conn` per collector;
polls in a background goroutine, one at a time, with a hard timeout of `max(10s, 2×interval)`;
server-side statement/lock/idle-in-transaction/idle-session timeouts and TCP keepalives
(PostgreSQL), `wait_timeout`/`lock_wait_timeout`/`max_statement_time`|`max_execution_time`
(MySQL/MariaDB); a session given up on is ended by the next connection (`pg_terminate_backend`
matched by pid + `backend_start`; `KILL CONNECTION` matched by id + user + server start) and no
poll runs while it lives; exponential backoff 1 s → 5 min; client TCP keepalive 15 s/5 s×3 for
lib/pq; sessions labelled `kula`. Verified against PostgreSQL 17, MariaDB 11 and MySQL 8.4
(settings applied, busy session reaped in ≤ 105 ms, mismatched identity untouched, `docker pause`
outage: peak ≤ 2 Kula sessions, polling resumed, 0 sessions after SIGTERM) and by fake-driver
tests (5,000-step churn never exceeds 1 client connection / 2 server sessions). Residual: each
running `kula tui` holds its own session per database. (The hard timeout is also capped at one
minute; the first backoff delay is one collection interval.) **Follow-up `0c3c6eb`:** a poll
stuck on a frozen server (its kernel still ACKs) or on unacknowledged data kept the lib/pq
goroutine blocked for minutes; its dialer now tracks connections and `dbSession.bound` closes
them once work outlives its deadline by 5 s.
- **[collector] `statfs` on NFS/CIFS runs inside `Collect()`** (`disk.go:433`); a hung export
  stalls all collection (plausible, not reproduced). **nginx/apache/pg/mysql are polled
  synchronously in `Collect()`** with timeout = interval (`collector.go:226-254`), so one tick
  can take 4–5× the interval. *Fix:* the async `Latest()` pattern already used for containers.
  **PostgreSQL/MySQL part ✅ fixed in `0721fc4`** (background pollers).
  **nginx/Apache2 ✅ fixed in `79b7ab6`:** the database collectors' poller moved to `poller.go` and
  runs the status pages too (request timeout `max(2s, 2×interval)`, result current for
  `max(3s, 3×interval)`, no redirects); a hung page leaves `Collect` untouched
  (`TestStatusPagesPollOffTheCollectionTick`: 2.1 s → immediate). `debugDone` became atomic, since
  pollers read it. **`statfs` ✅ fixed in `dc1868b`:** at most one call in flight per mount, a
  tick waits at most 200 ms, a stuck mount keeps its last reading for three intervals and then
  drops out, logged both ways (`mount_stats.go`).
- **[collector/perf] Per-tick rediscovery:** disk identity + hwmon walks per disk per second
  (`disk_identity.go:61-106`, `disk.go:462-550`); GPU discovery reruns every tick on GPU-less
  hosts (`gpu.go:20-22`); one Docker `inspect` per container per second
  (`containers.go:387-411`); `pg_database_size` + `pg_stat_user_tables` + `SHOW max_connections`
  + Ping every second (`postgres.go:172, 240-266`; the per-poll Ping is gone since `0721fc4`); container `prev*` maps never pruned
  (`containers.go:573, 622, 656`).
  **✅ GPU `a376d7e`** (a host without one looks again once a minute). **✅ Containers `a9cbd35`:**
  the PID from inspect is kept while `/proc/<pid>/cgroup` still names the container (a restart
  or reused PID inspects again), and a container that leaves the list, or every container when
  listing fails, loses its baselines; a failed per-container read drops that baseline too.
  **✅ `SHOW max_connections` `477d7ae`:** once per session (changing it needs a restart, which
  ends the session); verified on PostgreSQL 17: 2 reads in ~10 polls across a restart.
  **Measured, not changed:** identity resolution costs 0.15 ms per tick for 3 devices here; it is
  what notices a hot-swapped disk. The hwmon cost is not the walk (µs) but the read itself:
  each NVMe `temp*_input` read issues a SMART admin command, 2.3–4.5 ms per sensor (up to
  10 ms once the drive has idled). Measured at the real 1 s cadence, the larger cost was the
  battery: ACPI `_BST` through the embedded controller, 81 ms whenever the kernel's 1 s cache
  has expired (back-to-back reads had hit the cache), so a tick took 91 ms of wall time and
  6 ms of CPU. **✅ Disk temperatures and power supplies `a458c93`:** read every
  `collection.disk_temp_interval` / `collection.power_supply_interval` (default 10 s, a
  multiple of the interval; set to it to read every sample) and repeated in between, through
  `cadence.go`: one read in flight per key in its own goroutine, the starting tick waits
  `statfsWait`, a hung read is never doubled and drops its key after three intervals, logged
  once each way; a disk swapped in under a known name is read at once, unselected disks never.
  On this host 9 ticks in 10 now take 4.5–6 ms. **✅ PostgreSQL `21ded1c`:** the
  `pg_stat_user_tables` sums and `pg_database_size` once a minute (30 and 64 ms of server time
  per poll at 10 000 tables, flat below 0.2 ms for the other queries); verified on
  PostgreSQL 17 with `log_statement=all`: 3 reads in 126 polls, the size in every result.
- **[web/perf] `gzip.NewWriter` per request, allocated before the handler**
  (`server.go:228-235`) — ~600 KB each, even for 304/204. *Fix:* `sync.Pool` + lazy creation.
  **✅ Fixed in `cfd99c1`.** Compressors come from a `sync.Pool` and are taken on the first body
  byte or flush; a handler that writes nothing goes out unencoded. *Correction:* `gzip.Writer`
  allocates its deflate state on first write, so 304/204 never paid it; every compressed body
  did, about 1 MB. Measured: a small JSON response 1.09 MB / 142 µs → 18 KB / 8 µs allocated per
  request, `style.css` 1.26 MB → 219 KB. Same commit fixes the Low "gzip exclusion for
  `/api/ollama/` is dead under `base_path`".
- **[web] Access-log injection** via `%0a` in `r.URL.Path` (`server.go:165`); reproduced a fully
  forged `[API]` line. *Fix:* log with `%q`.
  **✅ Fixed in `7887406`** by logging `r.URL.EscapedPath()` instead (keeps the line format; the
  newline stays `%0a`). Header values, so `X-Forwarded-For`, cannot carry control characters
  past `net/http`. `TestAccessLogEscapesThePath` fails on the old code.
- **[frontend] WebSocket reconnect ignores a 401 upgrade rejection** (`websocket.js:83-97`): an
  expired session after sleep → infinite retries, no login overlay.
  **✅ Fixed in `c10dcad`.** A socket that closes before it opened asks `/api/auth/status`;
  an expired session returns to login, anything else (server down, 429) keeps backing off. A
  generation counter stops a check still in flight from acting after a login or logout. New
  browser fixture `session_expiry_test.mjs` (Frontend workflow, `test-frontend-regressions.sh`):
  real `kula serve` with auth, restarted without `sessions.json` under an open page; times out
  on the old client.
- **[frontend] `initOllama()` re-runs on every `kula-config-ready`** (`ollama.js:39-114`),
  stacking anonymous listeners (duplicate sessions per click, double delete confirm).
  **✅ Fixed in `b69d638`.** The panel is wired once; later loads only show the button. The
  model `<option>` is built with `textContent` instead of unescaped `innerHTML`. The dashboard
  fixture loads the config twice and requires one session per click.
- **[frontend] Alert list overwrites the translated empty state with English every second**
  (`alerts.js:87`).
  **✅ Fixed in `ae8ce32`** (translated, and keeps its `data-i18n` key for later language changes).
  The alert titles themselves are still English (the Low i18n item).
- **[tooling] `inspect_tier.py` has no hole resync** (`:940-956`): after an unclean shutdown it
  reports a stale "latest" record while `kula inspect` is right (reproduced: 30-minute
  discrepancy). Also lacks the 64 MiB cap and file-size clip.
  **✅ Fixed in `39dc78b`.** A `Ring` class mirrors `walkSegment` / `resyncToNextRecord`
  (two-in-a-row check, whole-record checksum, timestamp bounds) over an mmap, with the cap and
  the clip. Found on the way: the inspector also trusted a persisted tail Go rejects, and for
  files from before tail tracking started the old pass at the header's `oldest_off` instead of
  `write_off`; both mirrored. `TestInspectorWalksTheRingLikeTheReader` compares the records
  both visit over seven damaged tiers; each mirrored rule, removed, fails its scenario.

### Low

**storage**
- Query cache (`store.go:534-620`) almost never hits for the dashboard (exact-ns keys,
  `to = now`, evicted every tick by the live-edge sweep) — harmless, but not the saving the docs
  imply. **✅ `bda96c2`** (documented, not changed: `05-storage-engine.md` and the
  `QueryRangeWithMeta` comment say it serves concurrent identical requests and past windows).
- `defer s.mu.RUnlock()` inside the candidate loop (`store.go:617`) is correct only because
  every path returns — fragile. **✅ `57c2d01`** (`cacheHistoryResult` with its own lock scope).
- Unclamped narrowing casts `uint16(FDs)`, `uint8(UserCount)`, `uint16(NumCores)` in
  `appendFixed` while neighbouring fields are clamped. **✅** `UserCount` since `897d9b4`;
  cores and descriptors `a6fc9fa` (`clampUint16`, same bytes for every value that fits).
- `downsampleHistory` (`query_planner.go:227`) is test-only dead code. **✅ `fb5ff4e`** (moved
  into `store_test.go`: `source_budget_test.go` uses it as the reference result).
- After a restart, tier 1 lags > 2 min, so 3–12 h views silently drop to 5 m buckets until the
  first rollup lands (observed). *Open, likely narrower now (from the code, not measured):* since
  `e8c02e6` the restart replays the open window and the first sample flushes it, so only a
  downtime of roughly 2–10 minutes leaves tier 1 short of its tolerance, for at most a minute,
  and only for a fresh load (an open view's tier hint keeps tier 1).
  **✅ `6a79604`:** a coarse tier now covers the live edge as far as its rollups are final: the
  start of the window holding the source tier's newest record, when that is past its newest
  rollup (every earlier window has closed, the downtime ones with nothing to roll up), unless an
  older window is still buffered (a failed write). Measured on 2 h 20 min of samples, a restart
  and five minutes down: a 2 h 10 min view came from tier 2 (5 m) before, tier 1 now.
  `TestRestartAfterADowntimeKeepsTierOneAtTheLiveEdge` and a contract scenario pin it.
- **✅ `30c9d19`** (found while verifying): a crash between creating and renaming `tier_N.idx`
  left a temporary file nobody removed; `OpenTier` sweeps it.
- **✅ `f2589f4`** (from verification): read-only stores (the TUI) kept no seek index, so every
  TUI history query read the whole tier. They now build one on first use and extend it after
  each refresh; an unseen lap of the ring between refreshes is detected and rebuilt.

**codec**
- Apache2 version byte ≥ 3 consumes nothing and decodes garbage as custom metrics
  (`codec.go:1423-1516`), whereas PSU/disk-ID sections error. **✅ `9a1d725`**, with the same
  gap in PostgreSQL (`>= 2`) and MySQL (`>= 3`), which read a future version as their newest
  layout; mirrored in `inspect_tier.py`, the adding-metrics template refuses unknown versions.
  `TestDecodeRefusesUnknownSectionVersions` checks Go and Python.
- GPU is the only length-gated (fail-open) section (`codec.go:1094-1098`). Documented in
  `df5c50e`; behavior unchanged. *Won't fix:* a flag would need a new bit, and records from
  before the GPU section still need the length gate.
- The decode template in `docs/dev/14-adding-metrics.md:131-134` lacks the `need(1, …)` guard
  and would panic if copied. **✅ `5072b86`.**
- Custom-metric `value` is the only user-fed quantity stored as float32 (exact only to 2^24);
  undocumented. **✅ `9c4c3c3`** (documented in `09-custom-metrics.md`).

**web**
- Ollama proxy follows redirects (`ollama.go:448, 552, 734, 776`). **✅ `7ffc2a2`**: the URL is
  validated to be loopback, and a redirect could have left it; the error names the target.
- `origin_validation: false` + `allowed_origins` + auth → WebSocket readable cross-site with no
  warning. **✅ `78882f2`** (startup warning; `04-configuration.md` says to keep validation on).
- Config accepts `argon2.time/threads: 0` (panics on login), `session_timeout: 0`,
  `max_websocket_conns: 0`, unnormalized `allowed_origins`. **✅ `d9c54a3`:** refused at load
  (`kula hash-password` panicked too); origins normalized (case, trailing slash, default port),
  and an entry with a path, wildcard or no scheme stops startup.
- The `get_metrics` AI tool bypasses the 31-day / inversion guards (`ollama.go:933-958`).
  **✅ `dde14cb`** (`validateHistoryRange`, shared with `/api/history`; an unreadable bound is an
  error instead of a silent fallback to the last hour, and `from` defaults to an hour before `to`).
- Dead option `web.graphs.gpu_temp` (already reported for 0.20.0). **✅ `8a2f61b`**: sent in
  `/api/config` (the dashboard already read it); `"auto"` uses `max_value`, as no GPU limit is
  detected. Added to `config.example.yaml` and `04-configuration.md`.
- DNS-rebinding exposure with auth off (no Host allowlist). *Open (needs a design decision):* an
  opt-in allowlist protects only those who set it; a default-on loopback rule breaks a same-host
  reverse proxy that forwards `Host`.
  **✅ `f94a7ea`** (default on, decided by the maintainer): with auth disabled on a TCP listener,
  only `Host` names no public DNS can answer for are served — IP addresses, `localhost`,
  single-label names, `.local`/`.lan`/`.home`/`.localdomain`/`.internal`/`.home.arpa`, the
  machine's hostname, the hosts of `allowed_origins` and the new `web.security.allowed_hosts`
  (`"*"` turns it off, with a startup warning). Anything else gets a JSON 403, logged once per
  name; `/health` and `/status` stay open. Auth on and Unix sockets are unaffected. A proxy that
  forwards a public `Host` with auth off must list it (upgrade note in `07-authentication.md`).
  Verified live under Landlock.
- gzip exclusion for `/api/ollama/` is dead under `base_path` (`server.go:214`). **✅ `cfd99c1`.**

**collector**
- Unguarded `Total-Free-Buffers-Cached` (`cpu.go:425, 434`) can wrap on inconsistent meminfo
  snapshots; `Total - MemAvailable` would match modern tools. **✅ `ded8fba`** (clamped at 0,
  swap too; the formula is unchanged so history keeps its meaning).
- `prev > 0` idiom drops the first error burst for TCP errs/rsts/retrans (`network.go:262-270`).
  **✅ `05fdf8c`**: a baseline flag from the previous successful read; a failed read (or a
  counter that does not parse) drops it. Found on the way: the retransmit counter was read from
  the wrong file (H4 follow-up, `9420996`).
- `/proc/mounts` `\040` escapes not unescaped before `Statfs` (`disk.go:383-434`). **✅
  `69237b4`**, with the replacer `internal/sysinfo` already used, so the inventory's join by
  mount path now matches such mounts too.
- Standby replication-lag seconds grow without bound on an idle primary (`postgres.go:300-305`).
  **✅ `e8d9f60`:** 0 s once the standby has replayed all it received from a WAL receiver that
  is streaming (`pg_stat_wal_receiver`; its status is hidden without `pg_monitor`, so then a
  running receiver suffices); otherwise the time since the last replayed commit, as before, so
  a standby that lost its primary still grows once the receiver times out. Verified on a
  PostgreSQL 17 primary/standby pair, through Kula and as `pg_monitor` and unprivileged users:
  idle primary 0 (old expression 8.5 s and climbing), paused replay grows, frozen or stopped
  primary grows after `wal_receiver_timeout`, reconnection returns to 0. Same metric name, HELP
  text updated.
- Apache `ReqPerSec` / `BytesPerSec` are lifetime averages presented as current
  (`charts-data.js:651`, `ai.go:387`). **✅** Prometheus `036c3a6`: new
  `kula_apache2_accesses_per_second` and `kula_apache2_served_bytes_per_second`; the old
  `requests_per_second` / `bytes_per_second` stay, deprecated in their HELP text and the docs.
  Dashboard and AI context `6148b5b` use the current rates (both record versions store them).
- MySQL DSN built with `Sprintf` (`mysql.go:41-45`). **✅ `0721fc4`** (driver `Config`; PostgreSQL
  values quoted too).
- 1 MiB cap on `/containers/json` truncates at ~400 containers. *Correction:* the truncated JSON
  failed to parse, so past the cap *every* container dropped out, silently. **✅ `c37963b`**
  (stream-decoded up to 64 MiB, a failure to list logged when it changes).
- Unsynchronized `sysTempSensors` global touched from the HTTP goroutine (`cpu.go:27, 205`).
  **✅ `2643cc0`** (per-collector `sync.Once`).
- Initial Postgres connect failures are never logged (**✅ `0721fc4`**, logged on change);
  nginx follows redirects; nginx/apache failures logged only on the first cycle (**✅ both
  `79b7ab6`**: no redirects, failures logged on change and on recovery).

**frontend**
- Stored `kula_aggregation` pref interpolated unvalidated into a CSS selector (`main.js:480`) —
  a stale value blanks the dashboard. **✅ `4c990d8`**, with `web.default_aggregation`, which the
  server does not validate either; the live Chromium fixture loads a malformed preference.
- Second undocumented Chart.js private dependency `TimeScale._unit` (`charts-init.js:170`).
  **✅ `8969984`** (documented; `TestChartBundle` asserts it).
- Full chart build runs twice at startup (`main.js:393` + `auth.js:109`). **✅ `33e8899`:** the
  second build only applied the four Graph Bounds ceilings from `/api/config`; charts created
  with a `graphBound` now take them in place (`applyChartBounds()`), as does a bound applied
  from a card's menu, which also rebuilt every chart and re-fetched history. One build measured
  60–80 ms on the review host, 430–475 ms with Chromium's CPU throttled 6×.
- O(total points) scan per update in the accessibility plugin (`chart-accessibility.js:446-455`).
  **Partly ✅ `0eceeba`:** one pass instead of two, ending at the first unavailable point. Still
  O(points) per update. *Won't fix (measured):* 0.3–0.5 ms per tick for 40 charts × 4 series ×
  ~900 points, 2.9 ms only at 3,600 raw points per series; live data changes every tick, so a
  per-change cache would not help the live view.
- `_sanitize()` in `split.js:306-308` can collide `eth0.1` / `eth0_1` into one card ID and abort
  the rebuild. **✅ `c6f2fef`**: a later name in sorted order gets `-2`, `-3`, …; a name that
  collides with nothing keeps its id, so stored card orders still apply.
- Split cards reimplement expand / hover-pause / bounds menus instead of sharing
  `chart-card-actions.js`. **✅** Already shared at the time of §4's check
  (`attachDynamicChartCardActions`).
- Hardcoded English strings bypass i18n (`alerts.js`, `header.js`, `ollama.js`, `auth.js`,
  `disk-identity.js`). **✅ `7179daf`:** 25 keys in all 26 locales; `i18n.format` fills
  `{name}` placeholders (a `$` stays literal); `disk-identity.js`, which the node tests load
  without imports, is handed the translator. The login form also tells a wrong password (401)
  from a rate limit (429) and a busy server (503), which all read "Invalid credentials"
  before; those messages fall back to English, as `/api/i18n` needs a session. *Still English:*
  about 110 application-chart titles, legends and subtitles in `charts-data.js` (nginx,
  Apache2, PostgreSQL, MySQL, power supplies), and abbreviations such as `usr:`/`sys:`/`estab:`.
  **✅ `315dc91`:** 72 keys in all 26 locales — card titles ("Nginx — <metric>"), plain-language
  legends and subtitle words, battery status, the disk-space legend. Technical terms sysadmins
  keep in English stay plain labels (Commits/s, Keepalive, DNS, Blks, Tuples, Bufs, InnoDB
  Buffer Pool, Background Writer, SQL verbs), as do abbreviations. Each locale keeps its own
  per-second unit and Disk I/O wording. Legends, titles and subtitles follow a language change
  without a rebuild, paused views included.
- CSP `style-src 'unsafe-inline'` appears unnecessary (`server.go:269`) — verify `game.js` first.
  **✅ `ec39733`:** `style-src 'self'`. The Chromium fixtures watch `securitypolicyviolation`
  through a CDP binding (`watchCSPViolations`) and fail on any; the dashboard harness serves
  `style-src 'self'` alone, and the live fixture covers the game. A deliberate
  `setAttribute('style')` and inline `style=` were both caught.

### Nits

- Stale header-layout comment in `tier.go:18-30` (`[4:8]` and `[56:64]` are not reserved).
  **✅ `14a80da`.**
- Stray `)` in `codec.go:26-29` (**✅ `df5c50e`**); Postgres decode comment omits v3
  (**✅ `9a1d725`**).
- `utUserOffset = 8` actually addresses `ut_line` (`system.go:100`); count still right.
  **✅ `897d9b4`** (offset 44).
- `appendVariable(nil)` writes an undecodable shape (unreachable). **✅ `e93aeb2`** (an error).
- Interface order in `/api/current` changes every tick (map iteration). **✅ `6acdf35`.**
- `http.Error` vs `jsonError` inconsistencies (**✅ `5fd41c6`** for `/api` and `/ws`); no
  `Cache-Control` on `/api/current|history|config` (**✅ `04e25c3`**, `no-store` on the API mux).
- WebSocket backoff without jitter; initial WS write has no deadline. **✅ `2550264`.**
- Unix sockets created at umask then chmod'ed (brief window). *Won't fix:* under the usual
  umask 022 the socket starts at 0755 (owner-only connect), tighter than the final 0660; a
  process-wide umask around `Listen` would also tighten files other goroutines create then.

---

## 4. Consistency and documentation mismatches

- **Advertised but wrong:** disk utilization (`docs/user/01-introduction.md:30`,
  `06-tui.md:60`, `11-prometheus.md:73`); utmp-based user count (`docs/dev/04-collector.md:32`);
  "counter resets on nginx restart handled" (only `accepts`); `https://` status URLs supported
  (`docs/dev/09-sandbox.md`).
  **✅ All four true now**, by the code fixes: `d8dbf29` (disk utilization), `897d9b4` (the
  collector doc names utmp and logind), `8b8980c` (nginx), `72d4640` (`https://`).
- `docs/dev/07-web-server-api.md`: "errors via `jsonError`" — auth 401, CSRF 403, WS 429 and
  `/api/` 404s are plain text. `docs/user/07-authentication.md`: Bearer API access —
  `/api/auth/status` and `/api/logout` are cookie-only.
  **✅ Both fixed in code, not in the docs.** `5fd41c6`: the auth 401, the CSRF 403s, a JSON 404
  for unknown `/api/` paths, and every refused `/ws` handshake — the connection-limit 429s and,
  through `Upgrader.Error`, gorilla's own 400/403 (still with `Sec-Websocket-Version: 13`) —
  answer `{"error":…}` as `application/json`. The doc lists what stays plain text (`/metrics`,
  static assets); `TestRuntimeErrorsAreJSON` drives each refusal and fails on the old code at
  all seven. `df7f018`: `/api/auth/status` and `/api/logout` honor the bearer header (logout
  revokes it; `TestRuntimeBearerSessionStatusAndLogout`). The doc now says the token is the
  `kula_session` cookie value, that header-only requests need no CSRF token, and that `POST`
  still needs `Origin`/`Referer` under `origin_validation`.
- `docs/dev/10-frontend.md`: `chart-card-actions.js` "shared by split cards" (only
  `addSaveImageButton` is); live-queue drain location; only `_updateHiddenIndices` listed as the
  private Chart.js contract (`_unit` also).
  **✅** The split-card claim had already become true (`split.js` uses
  `attachDynamicChartCardActions`). `8969984`: `websocket.js` queues live samples and
  `charts-data.js` `drainLiveQueue` drains them (module table and file header); `_unit` is
  documented next to `_updateHiddenIndices`, in AGENTS.md too. `TestChartBundle` now asserts
  that both exist, in the vendored bundle and in the upstream trio `replace_chartjs.sh`
  installs (run against Chart.js 4.5.1). Before, nothing failed when they were missing:
  `_updateHiddenIndices` is called as `?.()`, and a missing `_unit` makes every tick show HH:MM.
- `docs/dev/06-storage-codec.md` is numerically exact but omits: the three-way record dispatch
  (kind byte / JSON / legacy binary), bit 11 being terminal for future record-level extensions,
  GPU's length gating, and that version growth is backward-only.
  **✅ `df5c50e`:** a table of the four record kinds, GPU's length gating, the statistics trailer
  having to end the record, and "Forward compatibility" renamed "Reading older records", with
  what an older binary does with an unknown flag (raw: ignored; rollup: misaligned) or version
  (per section). `05-storage-engine.md` and `12-backups.md` also called the codec
  forward-compatible; corrected. *Found on the way:* a pre-0.11.0 binary record has no kind
  byte and starts with its timestamp's low byte, so ≈3/256 of them match a tag and are
  misread or skipped. Documented, not fixed: only records old enough to have left most rings.
- `sandbox.go:42-44` comment says RO for the container socket; code and `docs/dev/09` say RW.
  **✅** Says RW since `a77f87e`.
- `docs/user/04-configuration.md`: `trust_proxy` described as X-Forwarded-Proto only; it also
  governs X-Forwarded-For.
  **✅ `8bdbb8d`:** `04-configuration.md`, `config.example.yaml` and `13-reverse-proxy.md`
  name both headers, what each sets, and when enabling it lets a client choose its own IP.
- Otherwise consistent: defaults agree across `config.go`, `config.example.yaml` and
  `docs/user/04`; the middleware chain is identical on every route; collectors are uniformly
  nil-on-failure.

---

## 5. Verified correct (no need to re-check)

- **Routes/CSRF/sessions:** every `/api/*` route except login/logout/auth-status goes through
  CORS → Auth → CSRF → logging; `/ws` identical; `/metrics` bearer check is constant-time.
  Origin required on mutating requests; synchronizer token constant-time; 256-bit tokens,
  SHA-256 at rest, `sessions.json` 0600, sliding + absolute expiry, WS re-validation every 30 s.
- **WebSocket hub:** origin checks, read limit, deadlines, ping, global and per-IP caps,
  close-ownership under one lock, no goroutine leaks, one `json.Marshal` per tick, slow clients
  dropped without blocking collection.
- **Static serving:** traversal-proof (ServeMux cleaning + `embed.FS`), SRI over minified bytes,
  per-request CSP nonce, correct 304 under gzip.
- **Frontend sinks:** no XSS found; all `innerHTML` sinks constant or escaped; Ollama markdown
  escapes first and emits fixed tags only; storage access via `prefs.js`; chart teardown detaches
  listeners/observers; history requests are latest-wins with abort.
- **Collectors:** byte-level parsers with field-count guards, no panics on malformed input,
  monotonic `elapsed` with `<= 0 → 1`, reset guards on nearly every delta, no data races between
  the loop and app collectors, every `os.Open` closed, parameterized SQL, credentials never
  logged, bounded HTTP bodies.
- **Backups:** per-file consistent (copied under the tier read lock; since `ebae4e4` copied
  unlocked, then patched under it), temp-dir + rename, fsync,
  0750/0600 (not cross-tier atomic — acceptable, undocumented).
- **gen-mock-data** writes through the production `Store`, so its tiers are genuine codec and
  aggregation output.

---

## 6. Suggested order of work

> **Status (2026-11-08):** all five High findings are fixed — H1 `37c2d26`, H2 `3aeeaf8`,
> H3 `d8dbf29`, H4 `ffea358`, H5 `6bca6df` — and S1 `cbfb3e2`, S2 `1b32a06`, S3 `e8c02e6`, plus a
> wrap bug found on the way (S1b, `1e16482`). From "Medium — other subsystems": utmp `897d9b4`,
> CA roots `72d4640`, rootless Podman `a77f87e`, nginx `8b8980c`, Postgres `45c94bd`, MySQL
> `62592c3`, database sessions `0721fc4`. §7 records the verification of those fixes and the
> follow-ups it led to. S5 (record checksum) `85b5483`, with a resync hang found on the way
> (S5b, `838e22c`); S4 (backup lock) `ebae4e4`, `f0b93a3`. Still open: synchronous
> nginx/Apache2 polls and `statfs`, per-tick rediscovery, the web/frontend/tooling Mediums
> (gzip writer pool, access-log injection, WebSocket 401 reconnect, `initOllama`, alert i18n,
> `inspect_tier.py` hole resync), and the Low list. §4 (documentation mismatches) is closed:
> `5fd41c6`, `df7f018`, `8969984`, `8bdbb8d`, `df5c50e`.
> **Since then:** access-log injection `7887406`, config limits `d9c54a3`, WebSocket 401
> `c10dcad`, `initOllama` `b69d638`, alert i18n `ae8ce32`, gzip pool `cfd99c1`,
> `inspect_tier.py` ring walk `39dc78b`, nginx/Apache2 background polls `79b7ab6`, `statfs`
> `dc1868b`, GPU rediscovery `a376d7e`, container inspect and pruning `a9cbd35`, PostgreSQL
> `max_connections` `477d7ae`, disk temperature and power supply intervals `a458c93`,
> PostgreSQL relation statistics once a minute `21ded1c`. No Medium is open. Gate passes at
> `21ded1c`; the four browser fixtures last ran at `a8af522` (no frontend change since).
> **Lows (2026-11-09):** 23 small fixes and the Apache2 decision, `a6fc9fa`…`6148b5b`, marked
> in §3; one bug found on the way (the retransmit counter's source, H4 follow-up `9420996`).
> Then replication lag `e8d9f60`, CSP `ec39733`, translations `7179daf`.
> **Last open items (2026-11-09):** startup double chart build `33e8899`, restart/5 m buckets
> window `6a79604`, application-chart strings `315dc91`, DNS rebinding `f94a7ea` (default on).
> Nothing is open. Won't fix: GPU length gating, the Unix socket chmod window, caching the
> accessibility scan (measured, below 1 ms per tick). Found on the way: split-card titles and
> legends, and a sensorless device's temperature legend, ignored a language change
> (`0117ba9`); system-info device names were English (`5b67f68`); one Portuguese string was
> Spanish (`d1d1222`).

1. ~~**H1 + H2** (auth) — small changes, high impact.~~ ✅
2. ~~**H4** (`retrans_ps`: bit 13 + Python mirror) and **H3** (disk utilization: restore or remove)
   — the two archival-correctness bugs.~~ ✅
3. ~~Sandbox rules for utmp, CA bundle, rootless Podman; nginx/Postgres delta guards.~~ ✅
4. ~~**H5 + S1 + S2** — the performance items that matter on low-power targets (tail-only
   refresh, sparse offset index, single startup scan).~~ ✅
5. ~~**S3** — align rollup windows to the epoch.~~ ✅ (the clip-right path stays for a `to`
   that is not on a minute, but now re-buckets aligned records one to one)
6. ~~Frontend Mediums (401 reconnect, `initOllama` idempotence, alert i18n)~~ ✅, then the Low list
   opportunistically.

---

## 7. Verification of the fixes (2026-11-08)

Every commit marked ✅ above was re-read against the code at `fd2b08e`, with the relevant
tests run and the riskiest claims checked by mutation or a live run:

- **Gate:** `gofmt`, `go vet`, `go test -race ./...` and `golangci-lint` clean; the semgrep
  workflow (586 rules, run locally in the CI container) reports 0 findings.
- **Mutation:** reverting S1b reproduces the 23 s hole; dropping the retransmit encode makes
  `TestCodecPersistsEveryAggregatedNumericField` name exactly that field in all three blocks; a
  one-point seek error fails `TestSeekIndexScansMatchFullScans`.
- **Live:** with auth off `/api/login` answers 404 under the default sandbox, and a clean
  shutdown writes all three `tier_N.idx`; utmp offsets checked against `/run/utmp`.
- **Frontend:** a probe compared tail and full refreshes against real stores over ten
  scenarios (2–15 s steps, outages, catch-up, clock back-step, collector lag); the Chromium
  suite passes.

All fifteen fixes were implemented as described. The verification found the gaps below; each
was fixed in its own commit:

| Gap | Commit |
|---|---|
| Apache2 kept a stale baseline after a failed poll (doubled rates in history) | `b4dacc2` |
| One lagging tail left a rolling view "partial" for up to five minutes | `051912b` |
| An empty utmp pre-empted the logind fallback and read 0 users | `8971a38` |
| Ollama URL without a port allowed on 11434; dead `PGSSLCRL` rule | `01b3a55`, `c4ba249` |
| lib/pq could stay blocked on a frozen server long past the poll limit | `0c3c6eb` |
| Argon2id bound counted hashes, not memory | `d8f0fe4` |
| Sessions from older builds for a configured name survived upgrade-and-enable | `f280b64` |
| Crashed sidecar saves left temporary files behind | `30c9d19` |
| Decode template in the adding-metrics guide could panic if copied | `5072b86` |
| S3 (recommended next) | `e8c02e6`, `4c3974c` |
| TUI history read the whole tier per query | `f2589f4` |
| Views ≥ 3 h re-fetched every bucket each refresh | `30b1cae` |

Documentation drift found on the way (poll-timeout cap, first backoff delay, `PGSSLCRL`, the
"byte-identical" tail wording) was corrected in the same commits.

