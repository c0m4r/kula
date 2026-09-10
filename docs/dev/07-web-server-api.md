# Web Server & API

Package: [`internal/web`](../../internal/web/)

The web server hosts the dashboard SPA, the JSON REST API, the WebSocket live stream, the
Ollama proxy, the Prometheus exporter, and the health endpoints — all from one HTTP server with
a shared middleware chain.

## Files

| File | Role |
|------|------|
| [`server.go`](../../internal/web/server.go) | HTTP server, listeners, routing, middleware, templates, SRI, config endpoint |
| [`history_sections.go`](../../internal/web/history_sections.go) | Optional history response section filtering |
| [`auth.go`](../../internal/web/auth.go) | Argon2id hashing, sessions, rate limiting, CSRF, Origin validation |
| [`websocket.go`](../../internal/web/websocket.go) | WebSocket upgrade, broadcast, pause/resume, connection limits |
| [`prometheus.go`](../../internal/web/prometheus.go) | `/metrics` exposition + bearer auth |
| [`ollama.go`](../../internal/web/ollama.go) | Ollama/OpenAI-compatible AI proxy with tool calling |

## Listeners

`Server.Start()` opens either:

- **Dual-stack TCP** (IPv4 + IPv6) on `web.listen:web.port`, or
- a **Unix domain socket** at `web.unix_socket` (with `unix_socket_mode`), in which case the TCP
  listener is not opened. Stale sockets are safely removed first (after confirming nothing is
  listening).

`Server.Shutdown(ctx)` saves sessions to disk and gracefully stops the HTTP server (called with
a 5-second timeout on `SIGINT`/`SIGTERM`).

## Routing & base path

Routes are registered on an inner mux, then wrapped so everything is served under
`web.base_path` if set (`http.StripPrefix`). When `web.ui` is false, only `/metrics` and
`/health`/`/status` are registered.

| Method | Route | Handler | Auth |
|--------|-------|---------|------|
| GET | `/`, `/index.html` | dashboard SPA | — |
| GET | `/api/current` | latest sample (`Collector.Latest()`) | yes¹ |
| GET/HEAD | `/api/system-info` | current hardware inventory and utilization | yes¹ |
| GET | `/api/history` | time-range history | yes¹ |
| GET | `/api/config` | UI config (theme, langs, graph bounds, custom metrics, ollama) | yes¹ |
| POST | `/api/login` | login | public |
| POST | `/api/logout` | logout | public |
| GET | `/api/auth/status` | whether auth is on / logged in | public |
| GET | `/api/i18n?lang=` | locale strings | yes¹ |
| POST | `/api/ollama/chat` | AI chat (SSE stream) | yes¹ |
| GET | `/api/ollama/models` | list local Ollama models | yes¹ |
| GET/POST | `/api/ollama/context` | per-chart context bootstrap | yes¹ |
| GET | `/ws` | WebSocket live stream | yes¹ |
| GET | `/metrics` | Prometheus exposition | bearer (optional) |
| GET | `/health`, `/status` | liveness (`200 kula is healthy`) | public |
| GET | static: `/js/`, `/fonts/`, `/style.css`, `/kula.svg`, `/favicon.ico`, `/game.*` | embedded assets | — |

¹ Protected by `AuthMiddleware` only when `web.auth.enabled` is true; otherwise open.

`/api/login`, `/api/logout`, and `/api/auth/status` go through the CORS middleware but **not**
the auth middleware (you must reach them while logged out). Everything under `/api/` else goes
through `corsMiddleware → AuthMiddleware`.

## Middleware chain

`securityMiddleware` (headers) → gzip (if `enable_compression`) → logging (`[API]`/`[WEB]`
tagged) → CORS → auth/CSRF. Security headers, CSP nonce, and SRI behavior are detailed in
[Security Model](08-security.md).

## REST API details

### `GET /api/current`

Returns the latest `Sample` as JSON. `503 no data yet` before the first sample.

### `GET /api/system-info`

Returns a current snapshot with `ts`, optional `metrics_ts` and `live`, and the inventory
sections `system`, `board`, `bios`, `cpu`, `memory`, `dimms`, `disks`, `filesystems`, `network`,
`pci`, `usb`, `sensors`, and `power`. Hardware attribute maps omit unreadable values;
optional numeric fields are omitted when unknown. The response is available before the first
metric collection; `live` and `metrics_ts` are then absent. `live` contains selected fields
from `Collector.Latest()`, never storage. Time-range parameters do not select historical data.

`internal/sysinfo.Provider` serializes discovery and shares an immutable snapshot for five
seconds across clients. It starts no background workers. Disk/network rate baselines are
discarded after idle gaps longer than 15 seconds, disappearing devices, identity changes,
or decreasing counters. Network `rx_pct` and `tx_pct` use each direction's rate divided by
the reported link speed; unknown speed leaves both absent. Disk `busy_pct` uses the delta
of active milliseconds in sysfs block statistics.

Responses send `Cache-Control: no-store`. Disabled `global.show_system_info` returns 404;
methods other than GET/HEAD return 405. Existing API authentication, base paths, and UI
enablement apply. Discovery uses the existing read-only `/proc` and `/sys` sandbox rules;
OS metadata is supplied from startup configuration. No storage schema/codec changes occur.

Sources: [Linux block statistics](https://docs.kernel.org/block/stat.html),
[network sysfs ABI](https://github.com/torvalds/linux/blob/master/Documentation/ABI/testing/sysfs-class-net),
and [DMTF SMBIOS specification, section 7.18](https://www.dmtf.org/sites/default/files/standards/documents/DSP0134_3.9.0.pdf).

### `GET /api/history?from=&to=&points=&sections=`

- `from`, `to` — RFC 3339 timestamps. Defaults: `to = now`, `from = to − 5m`.
- `points` — desired data points, default `450`, **capped at 5000** (min 1).
- `sections` — optional comma-separated top-level sample sections. Accepted values are
  `cpu,lavg,mem,swap,net,disk,sys,proc,self,gpu,psu,apps`. Unknown names return `400`.
- Inverted ranges and all windows over 31 days return `400`.
- Returns `{ samples, tier, resolution, source_resolution, downsampled, requested_from, requested_to,
  actual_from, actual_to, complete, exact_complete, valid_aggregations }`. When `sections` is supplied, the same compatible envelope
  also includes the canonical ordered `sections` list, and each sample's Data/Min/Max tree
  contains only its timestamp plus those sections. Omitting the parameter preserves the full
  response for existing clients.

  Each item in `samples` contains `ts`, `dur`, `data`, optional trusted `min`/`max`, and
  query-only presentation metadata: `bucket_start`, `bucket_end`, `sample_count`, and
  `coverage`. `sample_count` is the exact number of immediate records from the selected source
  tier that contributed to that output bucket (not an inferred raw-observation count).
  `coverage` is their summed observed duration divided by the bucket width, clamped to `[0,1]`.
  Source timestamps mark interval **ends**. History includes overlapping source intervals
  even when their endpoints fall after `to`. When downsampling, boundary-crossing intervals
  contribute proportionally to each overlapping output bucket; sums and weights are scaled
  together. A source record can therefore contribute to two buckets. Raw interval widths use
  their recorded duration; coarse records use native resolution because their duration is
  contributing weight, not a precise wall-clock span. Coarse boundary allocation and extrema
  remain limited to source resolution; this does not reconstruct missing raw observations.
  The selected source `tier`, its native `source_resolution`, and effective output `resolution`
  remain response-level metadata; section filtering preserves them and all bucket fields.

  The store chooses source quality with a fixed 7,500-record target independent of display
  density, then uses stable epoch-aligned output buckets and returns no more than `points`.
  Denser sources are processed in bounded decode batches, retaining all observations and
  their extrema. A 30-day range is supported even when it exceeds that selection target;
  the target does not cause HTTP 422 or silently truncate history.
  `downsampled` means records were reduced for presentation, including when `tier` is `0`.
  Usually output resolution exceeds native resolution; excess jittered observations can
  also require reduction at the native step, as does clipping an interval ending after `to`.
  Clients must not equate tier `0` with raw output.
  `complete` describes retention selection, tolerating one source interval at the left edge
  and two at the live edge for collection/rollup lag. `actual_from` and `actual_to` describe the
  represented source bounds **clipped to the request**, before presentation downsampling, and
  are omitted when no source interval overlaps it. `exact_complete` requires those bounds to
  reach both requested edges without tolerance. Neither completeness flag guarantees absence
  of interior gaps; inspect timestamps and per-bucket coverage as well. Exact/custom views use
  `exact_complete`; rolling live views tolerate normal lag using `complete`.
  `valid_aggregations` lists the
  envelope fields that are valid across every metric in the response. Trusted policy-reducer
  buckets return `["data","min","max"]`; raw points and legacy rollups return `["data"]`.
  Clients must honor this list because old tier files remain readable but their historical
  Min/Max blocks are intentionally not trusted. At `perf`/`debug` log level the chosen tier,
  effective resolution, sample count, and load time are logged.

### `GET /api/config`

Returns UI configuration: `auth_enabled`, `join_metrics`, OS/kernel/arch, hostname,
`show_system_info`, `show_version`, theme, aggregation, per-graph bounds (`cpu_temp`,
`disk_temp`, `network` with `mode`/`value`/`auto`-detected limit), split toggles, language
config, `ollama_enabled`/`ollama_model`, custom-metric definitions, and (if shown) version.

`history` contains `collection_interval_ms` and `ranges: [{from,to}, ...]` for nonempty
storage tiers. These are inexpensive interval envelopes derived from in-memory tier headers
(the oldest end timestamp is extended left by that tier's native resolution),
not an index of every available day or proof of gap-free data. The date picker refreshes them
on opening; no retention scan or new endpoint is involved.

### `GET /api/i18n?lang=`

Returns the translation map for the requested language; junk/traversal language codes are
rejected.

### Error responses

Errors are emitted via `jsonError`, which uses `json.Marshal` (not `fmt.Sprintf`) to prevent
JSON injection.

## WebSocket (`/ws`)

[`websocket.go`](../../internal/web/websocket.go):

- Upgrades only same-origin requests (Origin validation; non-browser clients without an Origin
  header are allowed). Cross-origin upgrades are rejected (CSWSH protection).
- Enforces a **global** connection cap (`max_websocket_conns`, default 100) and a **per-IP** cap
  (`max_websocket_conns_per_ip`, default 5).
- `Server.BroadcastSample(sample)` fans the latest sample out to all non-paused clients.
- A **read pump** accepts JSON control commands: `{"command":"pause"}` and
  `{"command":"resume"}` (the dashboard auto-pauses while you zoom). Incoming messages are read
  with a 4096-byte limit and a 60-second deadline refreshed by pong handlers.
- Unregister is guarded by `sync.Once` to avoid double-decrementing the connection counters.

## Prometheus (`/metrics`)

[`prometheus.go`](../../internal/web/prometheus.go) renders all metrics in text exposition
format, with all series prefixed `kula_` and per-device labels. Optional bearer-token auth
(constant-time compare). See [Prometheus Exporter](../user/11-prometheus.md) for the metric
catalog.

## Ollama proxy (`/api/ollama/*`)

[`ollama.go`](../../internal/web/ollama.go) is an OpenAI-compatible proxy to a **local** Ollama:

- `handleOllamaChat` — streams a chat completion as SSE, running an agentic tool-calling loop
  where the model can call `get_metrics` (≤5 rounds), backed by `Collector.FormatForAI()`.
- `handleOllamaModels` — lists locally available models.
- `handleOllamaContext` — bootstraps a per-chart analysis session with recent data as CSV.

All three apply prompt sanitization, model-name validation, rate limiting, and body/response
size caps. See [AI Assistant](../user/10-ai-assistant.md) and [Security Model](08-security.md).

## Templates, SRI & embedding

`server.go` renders the HTML templates at request time, injecting a fresh CSP nonce per request
and `integrity="sha384-..."` SRI attributes computed at startup (`calculateSRIs`,
`sha512.Sum384`). The SPA, fonts, and icons are embedded with `//go:embed static`.

Next: [Security Model](08-security.md).
