# Web Server & API

Package: [`internal/web`](../../internal/web/)

The web server hosts the dashboard SPA, the JSON REST API, the WebSocket live stream, the
Ollama proxy, the Prometheus exporter, and the health endpoints — all from one HTTP server with
a shared middleware chain.

## Files

| File | Role |
|------|------|
| [`server.go`](../../internal/web/server.go) | HTTP server, listeners, routing, middleware, templates, SRI, config endpoint |
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

### `GET /api/history?from=&to=&points=`

- `from`, `to` — RFC 3339 timestamps. Defaults: `to = now`, `from = to − 5m`.
- `points` — desired data points, default `450`, **capped at 5000** (min 1).
- Window **capped at 31 days**; inverted ranges → `400`.
- Returns `{ samples, tier, resolution, requested_from, requested_to, actual_from, actual_to,
  complete }`. The store prefers a tier covering the full request and downsamples to no more
  than `points`. `complete` is false when only part of the requested range is retained;
  `actual_from` and `actual_to` describe the returned bucket coverage (which can extend by one
  bucket at the left edge) and are omitted when no data exists. At `perf`/`debug` log level the
  chosen tier, resolution, sample count, and load time are logged.

### `GET /api/config`

Returns UI configuration: `auth_enabled`, `join_metrics`, OS/kernel/arch, hostname,
`show_system_info`, `show_version`, theme, aggregation, per-graph bounds (`cpu_temp`,
`disk_temp`, `network` with `mode`/`value`/`auto`-detected limit), split toggles, language
config, `ollama_enabled`/`ollama_model`, custom-metric definitions, and (if shown) version.

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
