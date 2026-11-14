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
| [`history_response.go`](../../internal/web/history_response.go) | History query slots and the streamed history response |
| [`system_info.go`](../../internal/web/system_info.go) | `/api/system-info` handler and hardware inventory response |
| [`auth.go`](../../internal/web/auth.go) | Argon2id hashing, sessions, rate limiting, CSRF, Origin validation |
| [`websocket.go`](../../internal/web/websocket.go) | WebSocket upgrade, broadcast, pause/resume, connection limits |
| [`prometheus.go`](../../internal/web/prometheus.go) | `/metrics` exposition + bearer auth |
| [`ollama.go`](../../internal/web/ollama.go) | Ollama/OpenAI-compatible AI proxy with tool calling |

## Listeners

`Server.Start()` opens either:

- **Dual-stack TCP** (IPv4 + IPv6) on `web.listen:web.port`, or
- a **Unix domain socket** at `web.unix_socket` (with `unix_socket_mode`), in which case the TCP
  listener is not opened. Stale sockets are safely removed first (after confirming nothing is
  listening). A socket whose mode cannot be set is closed (which removes it) and `Start` fails:
  left at the umask's mode it could admit clients `unix_socket_mode` keeps out.

`Server.Shutdown(ctx)` saves sessions to disk and gracefully stops the HTTP server (called with
a 5-second timeout on `SIGINT`/`SIGTERM`).

## Routing & base path

Routes are registered on an inner mux, then wrapped so everything is served under
`web.base_path` if set (`http.StripPrefix`). When `web.ui` is false, only `/metrics` and
`/health`/`/status` are registered.

| Method | Route | Handler | Auth |
|--------|-------|---------|------|
| GET | `/`, `/index.html` | dashboard SPA | — |
| GET/HEAD | `/api/current` | latest sample (`Collector.Latest()`) | yes¹ |
| GET/HEAD | `/api/system-info` | current hardware inventory and utilization | yes¹ |
| GET/HEAD | `/api/history` | time-range history | yes¹ |
| GET/HEAD | `/api/config` | UI config (theme, langs, graph bounds, custom metrics, ollama) | yes¹ |
| POST | `/api/login` | login (`404` while `web.auth.enabled` is false) | public |
| POST | `/api/logout` | logout | public |
| GET/HEAD | `/api/auth/status` | whether auth is on / logged in | public |
| GET/HEAD | `/api/i18n?lang=` | locale strings | yes¹ |
| POST | `/api/ollama/chat` | AI chat (SSE stream) | yes¹ |
| GET/HEAD | `/api/ollama/models` | list local Ollama models | yes¹ |
| GET/HEAD | `/api/ollama/context` | metrics snapshot a new AI session pins as its context | yes¹ |
| GET | `/ws` | WebSocket live stream | yes¹ |
| GET/HEAD | `/metrics` | Prometheus exposition | bearer (optional) |
| GET | `/health`, `/status` | liveness (`200 kula is healthy`) | public |
| GET | static: `/js/`, `/fonts/`, `/style.css`, `/kula.svg`, `/favicon.ico`, `/game.*` (when `global.easter_egg`) | embedded assets | — |

¹ Protected by `AuthMiddleware` only when `web.auth.enabled` is true; otherwise open.

An API route answers a method it does not serve with a JSON `405` whose `Allow` header names
the ones it does: `GET, HEAD` for the read-only routes (wrapped in `readOnly` where they are
registered; `/api/system-info` checks after its own `404`), `POST` for login, logout and the
chat. The check runs in the handler, after CORS, auth and CSRF, so a request those refuse
keeps their `401` or `403`, and a CORS preflight from an allowed origin is answered by
`corsMiddleware` before it. `/metrics` answers a plain-text `405` with the same `Allow`.

`/api/login`, `/api/logout`, and `/api/auth/status` go through the CORS middleware but **not**
the auth middleware (you must reach them while logged out). Everything under `/api/` else goes
through `corsMiddleware → AuthMiddleware`.

## Middleware chain

gzip (if `enable_compression`) wraps `securityMiddleware` (headers), then the base-path mount,
then, while auth is disabled on a TCP listener, the `Host` check against DNS rebinding (every
route but `/health` and `/status`; see [Security Model](08-security.md#dns-rebinding)); per
route the chain is CORS → auth → CSRF → logging (`[API]`/`[WEB]` tagged) → handler. Security headers, CSP nonce, and
SRI behavior are detailed in [Security Model](08-security.md).

## REST API details

### `GET /api/current`

Returns the latest `Sample` as JSON. `503 no data yet` before the first sample.

### `GET /api/system-info`

Returns a current snapshot with `ts`, optional `metrics_ts` and `live`, and the inventory
sections `system`, `cpu`, `disks`, `filesystems`, `network`, `pci`, `usb`, `sensors`, and
`power`. Hardware attribute maps omit unreadable values; optional numeric fields are omitted
when unknown. The response is available before the first metric collection; `live` and
`metrics_ts` are then absent. `live` contains selected fields from `Collector.Latest()`, never
storage. Time-range parameters do not select historical data.

`internal/sysinfo.Provider` serializes discovery and shares an immutable snapshot for five
seconds across clients. It starts no background workers. The hwmon/thermal sensors and the power
supplies are read no more often than the collector reads disk temperatures and power supplies
(`Collector.ReadingIntervals`: `collection.disk_temp_interval` and `power_supply_interval`, 10 s
by default), because an NVMe temperature read sends the drive a SMART command and a battery read
is an embedded-controller round trip; the snapshots in between repeat the last readings.
Discovery is best effort: unreadable
attributes are omitted rather than reported as measured zeros, filesystem space totals are
taken from the latest collector sample instead of re-statting mounts, and every disk,
filesystem, and interface carries a `tracked` flag saying whether the regular collector stores
its history. Interface entries carry the kernel name, driver, kind, MTU, and speed; IP and MAC
addresses are deliberately not collected. `Provider.Current` takes the `show_system_details`
flag: with it off, `disks`, `filesystems`, `network`, `pci`, `usb`, `sensors`, `power` (and
`live.hottest`, `live.gpu`) come back empty while `system`, `cpu`, and the remaining live
metrics are unchanged. Hidden sections are empty JSON arrays, never `null`, so the response
shape matches a full snapshot. A discovery without details reads none of the hidden sections:
the endpoint is unauthenticated while auth is off, and the sensor and battery reads cost the
device. A snapshot with details also answers a request without them; a request with details
after a summary-only snapshot discovers at once.

Responses send `Cache-Control: no-store`. Disabled `global.show_system_info` returns 404;
methods other than GET/HEAD return 405. Existing API authentication, base paths, and UI
enablement apply. Discovery uses the existing read-only `/proc` and `/sys` sandbox rules;
OS metadata is supplied from startup configuration. No storage schema/codec changes occur.

Sources: [Linux block statistics](https://docs.kernel.org/block/stat.html),
[network sysfs ABI](https://github.com/torvalds/linux/blob/master/Documentation/ABI/testing/sysfs-class-net),
and [DMTF SMBIOS specification, section 7.18](https://www.dmtf.org/sites/default/files/standards/documents/DSP0134_3.9.0.pdf).

### `GET /api/history?from=&to=&points=&sections=&tier=`

- `from`, `to` — RFC 3339 timestamps. Defaults: `to = now`, `from = to − 5m`.
- `points` — desired data points, default `450`, **capped at 5000** (min 1).
- `tier` — optional tier hint (a non-negative integer, else `400`). When that tier retains any
  of the window it is tried first, before the tier the window alone would select; a tier the
  store does not have is ignored, and `tier` in the response always names the tier used. The
  dashboard sends it when it refreshes the newest buckets of a view built from tier 1 or 2, so
  those few minutes are answered from the same tier rather than from raw data. The hint is
  followed only while what that tier retains of the window fits the 7,500-record target
  below; past it (`tier=0` on a month) the window is answered as if unhinted.
- `sections` — optional comma-separated top-level sample sections. Accepted values are
  `cpu,lavg,mem,swap,net,disk,sys,proc,self,gpu,psu,apps`. Unknown names return `400`.
- Inverted ranges and all windows over 31 days return `400`. A window ending in the future is
  accepted and keeps its bounds in the response; its source tier is chosen as if it ended now.
- At most two history queries run at once (`maxConcurrentHistory` in
  [`history_response.go`](../../internal/web/history_response.go)): each holds its whole result
  in memory until it is sent, up to 5000 buckets of Data, Min and Max samples, around 100 MB for
  a long window on a large host. A further request waits for a slot for up to 10 seconds
  (`historySlotWait`), or until its client goes away, and is then answered `503` with
  `Retry-After: 2`. Requests answered from the query cache wait like the others. The dashboard
  keeps one history request in flight at a time and sends a refused one again after
  `Retry-After` (see [Frontend](10-frontend.md#history-and-rendering)).
- The response is streamed: the samples are encoded one at a time and sent in pieces of about
  1 MiB (`writeHistoryJSON`), never built whole, because the JSON of a long window at full width
  is tens of megabytes, more than its decoded samples. The bytes are the ones `writeJSON` would
  send for the whole payload.
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
  Existing clients must honor this strict list. Compatibility-aware clients may instead use
  `available_aggregations`, which lists operations available for at least some fields/buckets.
  They must then check each sample's `extrema_profile` against the response's `extrema_profiles`
  map: `current: ["*"]`, `legacy`/`mixed`: exact supported scalar JSON paths, `none: []`.
  Unknown or missing profiles in this contract expose no extrema. Section filtering narrows
  these paths and available operations; it preserves bucket profiles and the strict list.
  No client may apply a partial profile to an entire sample or silently substitute Data for
  unavailable Min/Max. See [Storage Engine](05-storage-engine.md#aggregation-semantics) for
  legacy eligibility and field coverage. At `perf`/`debug` log level the chosen tier,
  effective resolution, sample count, and load time are logged.

### `GET /api/config`

Returns UI configuration: `auth_enabled`, `join_metrics`, OS/kernel/arch, hostname,
`show_system_info`, `show_system_details`, `show_version`, theme, aggregation, per-graph bounds
(`cpu_temp`, `disk_temp`, `network` with `mode`/`value`/`auto`-detected limit), split toggles, the
server-side defaults behind the customization menu (`appearance`, `accessibility` including the
`text_size_range`), language config, `ollama_enabled`/`ollama_model`, custom-metric definitions,
and (if shown) version.

`history` contains `collection_interval_ms` and `ranges: [{from,to}, ...]` for nonempty
storage tiers. These are inexpensive interval envelopes derived from in-memory tier headers
(the oldest end timestamp is extended left by that tier's native resolution),
not an index of every available day or proof of gap-free data. The date picker refreshes them
on opening; no retention scan or new endpoint is involved.

### `GET /api/i18n?lang=`

Returns the translation map for the requested language; junk/traversal language codes are
rejected.

### Error responses

Every error under `/api/` and `/ws` is a JSON object, `{"error":"<message>"}`, with
`Content-Type: application/json`: the handlers' own errors, the auth middleware's `401`, the CSRF
middleware's `403`, `404` for an unknown `/api/` path, `405` for a method a route does not serve,
and a refused WebSocket handshake (`403`
for a foreign Origin, `429` over a connection limit, `400` for a malformed upgrade).
`jsonError` writes them with `json.Marshal` (not `fmt.Sprintf`) to prevent JSON injection.
`/api/current` answers through `writeJSON`, which turns a sample that cannot be encoded (a NaN
or infinity) into a `500` instead of an empty `200`. `/api/history` streams its samples
(`writeHistoryJSON`): such a sample is a `500` while nothing has been sent yet, within the first
MiB of the body; after that the response can only be cut short, and the failure is logged.
`/metrics` and the static assets answer errors in plain text.
`TestRuntimeErrorsAreJSON` drives each refusal through the production handler chain.

Every response from the API mux carries `Cache-Control: no-store` (`noStore`): the data is live
and session-bound, so neither a browser nor a proxy should keep it. A handler may set its own
policy instead (the Ollama chat stream sends `no-cache`).

## WebSocket (`/ws`)

[`websocket.go`](../../internal/web/websocket.go):

- Upgrades only same-origin requests (Origin validation; non-browser clients without an Origin
  header are allowed). Cross-origin upgrades are rejected (CSWSH protection).
- Enforces a **global** connection cap (`max_websocket_conns`, default 100) and a **per-IP** cap
  (`max_websocket_conns_per_ip`, default 5).
- `Server.BroadcastSample(sample)` fans the latest sample out to all non-paused clients. A
  sample that cannot be encoded (a NaN or infinity) reaches no client; that is logged when it
  starts and when samples are sent again, not on every tick.
- A **read pump** accepts JSON control commands: `{"action":"pause"}` and
  `{"action":"resume"}` (the dashboard auto-pauses while you zoom). Incoming messages are read
  with a 4096-byte limit and a 60-second deadline refreshed by pong handlers.
- The **write pump** sends each broadcast with a 10-second write deadline and pings every 50
  seconds; the latest sample sent on connect has the same deadline. A failed write ends the connection and is logged only at `web.logging.level: debug`:
  the client went away or stopped reading, and the error cannot say which (with compression,
  gorilla reports both as a flate stream error).
- Unregister is guarded by `sync.Once` to avoid double-decrementing the connection counters.

## Prometheus (`/metrics`)

[`prometheus.go`](../../internal/web/prometheus.go) renders all metrics in text exposition
format, with all series prefixed `kula_` and per-device labels. Disk series label `device` with
the persistent ID (or `kernel:<name>` when none is available), and `kula_disk_info` reports the
current `kernel_name` and `identity_source` alongside it. Optional bearer-token auth
(constant-time compare). Samples are gathered per metric family and written family by family,
one `# HELP` and `# TYPE` each, since per-container series are produced container by container
(`TestMetricFamiliesAreGrouped`). `escapeLabel` makes label values valid UTF-8, as the format
requires: one invalid byte would fail the whole scrape. See
[Prometheus Exporter](../user/11-prometheus.md) for the metric catalog.

## Ollama proxy (`/api/ollama/*`)

[`ollama.go`](../../internal/web/ollama.go) is an OpenAI-compatible proxy to a **local** Ollama:

- `handleOllamaChat` — streams a chat completion as SSE, running an agentic tool-calling loop
  where the model can call `get_metrics` (≤5 rounds, ≤8 calls each), backed by `Collector.FormatForAI()`.
  Every `data:` payload is a JSON string (`writeSSE`), so no character of the model's output can
  end a line: unnamed events carry text, `tool_call` and `done` an empty string, `error` a message.
- `handleOllamaModels` — lists locally available models.
- `handleOllamaContext` — returns the latest sample formatted for the model (`FormatForAI()`),
  which the dashboard pins as a new session's context; a chart session sends the chart's CSV
  instead.

All three apply per-IP rate limiting and response-size caps; the chat path additionally caps the
request body, forwards only clamped user and assistant turns, and runs only a model the backend
lists (or the configured one). Every request to the backend goes through `ollamaTransport`,
which connects only to loopback addresses, and follows no redirect. See
[AI Assistant](../user/10-ai-assistant.md) and [Security Model](08-security.md).

## Templates, SRI & embedding

`server.go` renders the HTML templates at request time, injecting a fresh CSP nonce per request
and `integrity="sha384-..."` SRI attributes computed at startup (`calculateSRIs`,
`sha512.Sum384`). The SPA, fonts, and icons are embedded with `//go:embed static`.

With `web.minify_assets` (default on), [`minify.go`](../../internal/web/minify.go) minifies every
embedded `.css` and `.js` file except the vendored `*.min.*` ones once per process, before SRI
hashes are computed, and `handleStatic` serves the minified bytes (`readStatic`). Both
minifiers are dependency-free and only remove comments and whitespace; they never rename or
rewrite tokens. The JavaScript lexer keeps every line break automatic semicolon insertion could
depend on, and re-lexes its output to require the exact input token stream. That re-check shares
the lexer's regex-or-division decision, so the decision itself must be right: a `/` after a
keyword that precedes an expression (`return`, `typeof`, `default`, …) starts a regular
expression unless the word is a property name (`a.of / 2`), and so does a `/` after the `)` of an
`if`, `for`, `for await`, `while` or `with` head. In CSS, a comment is not whitespace: next to
whitespace it collapses with it, beside a delimiter it is dropped, and between two tokens that
could merge it stays as `/**/`. A hex escape keeps the one whitespace character that ends it. A
file either minifier cannot follow is logged and served unminified. `TestEmbeddedAssetsMinify`
requires every shipped asset to minify, and checks each stylesheet with `cssGapsPreserved`, which
uses no code from the minifier. `TestMinifiedScriptsParseInNode` syntax-checks the output with
node, and `FuzzMinifyJS` / `FuzzMinifyCSS` cover arbitrary input.

Static assets carry a weak `ETag` of the bytes they are served as (`staticETags`, computed at
startup after minification) and `Cache-Control: no-cache`: the browser keeps its copy but
revalidates on every use, so an unchanged binary answers `304 Not Modified` without a body, and
an upgrade is picked up at once even though asset URLs carry no version. The templated pages
(`index.html`, `game.html`) carry a per-request nonce, so they get no `ETag` and are sent with
`Cache-Control: no-store` (`renderTemplate`): neither the browser nor a shared cache may keep a
copy that would serve the same nonce twice (`TestPagesAreNeverStored`). With
`enable_compression`, responses that may be gzipped add `Vary: Accept-Encoding`; a 304 or 204 (or
a handler that writes nothing) goes out unencoded, and `.woff2` fonts and the `/api/ollama/` stream
(below any `base_path`) are never gzipped. Compressors come from a `sync.Pool` and are taken only
for a body, since each holds about 1 MB of deflate state.

Next: [Security Model](08-security.md).
