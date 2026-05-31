# kula-scan

**Active, black-box safeguard scanner for a running Kula instance.**

Where the unit tests, fuzz targets, and in-process runtime tests
([`internal/web/runtime_security_test.go`](../../internal/web/runtime_security_test.go))
verify Kula's defenses *in the lab*, `kula-scan` verifies them *in the field*: it
points at a live Kula URL and probes it over HTTP/WebSocket the way an attacker or a
browser would, then reports — per check — whether each safeguard actually holds in the
deployed configuration (behind your proxy, with your TLS, your base path, your config).

It imports nothing from Kula's `internal/` packages. Every assertion is made over the
wire, so it complements the in-tree tests rather than duplicating them.

```
go build -o kula-scan ./cmd/kula-scan
./kula-scan http://localhost:27960
```

`kula-scan` is a developer/operator tool. It is **not** built into the release binary or
Docker image (those build only `./cmd/kula/`), but it lives in the same module, so
`go vet ./...` and `go test ./...` cover it.

## Usage

```
kula-scan [flags] <target-url>
```

| Flag | Meaning |
|------|---------|
| `-username`, `-password` | Credentials to unlock authenticated checks (login, CSRF token, WebSocket auth). Optional. |
| `-base-path` | Base path if Kula is mounted under one (e.g. `/kula`). Auto-detected from a path in the target URL. |
| `-timeout` | Per-request timeout (default `10s`). |
| `-insecure` | Skip TLS certificate verification (self-signed test instances). |
| `-aggressive` | Enable disruptive checks (see below). |
| `-fuzz` | Enable blind fault-injection fuzzing (see [Fuzzing](#fuzzing--fault-injection)). |
| `-fuzz-iter` | Iterations per randomized fuzz probe (default 200). |
| `-seed` | PRNG seed for fuzzing (0 = random; the chosen seed is reported so any finding is reproducible). |
| `-only` | Comma-separated categories to run: `auth,csrf,cors,headers,traversal,metrics,ws,input,rate,dos,redirect,tls,bypass,fuzz`. |
| `-fail-on` | Minimum FAIL severity that forces a non-zero exit: `info\|low\|medium\|high\|critical` (default `high`). |
| `-json` | Emit findings as JSON. |
| `-no-color` | Disable ANSI colors (auto-disabled when stdout is not a TTY or `NO_COLOR` is set). |
| `-v` | Verbose: print each request/response. |

### Examples

```bash
# Quick non-destructive scan
kula-scan http://localhost:27960

# Full authenticated scan over TLS
kula-scan -username admin -password 'hunter2' https://mon.example.com

# Only header / traversal / CORS checks
kula-scan -only headers,traversal,cors http://10.0.0.5:27960

# Include disruptive checks (rate-limit + connection floods)
kula-scan -aggressive -username admin -password 'hunter2' http://localhost:27960

# Blind fault-injection fuzzing (reproducible with -seed)
kula-scan -fuzz -fuzz-iter 500 -username admin -password 'hunter2' http://localhost:27960
kula-scan -fuzz -seed 12345 -only fuzz http://localhost:27960   # replay a finding

# Machine-readable output for CI
kula-scan -json https://mon.example.com > report.json
```

### Exit status

- `0` — no failing safeguard at or above `-fail-on`.
- `1` — one or more safeguards FAIL at or above `-fail-on` (the IDs are printed).
- `2` — usage error (bad flag, unreachable target).

This makes `kula-scan` usable as a release/CI gate: stand up the instance, scan it, fail
the build if a safeguard regressed.

## What each status means

- **PASS** — the safeguard is present and behaves as expected.
- **FAIL** — the safeguard is missing or bypassable (a real finding).
- **WARN** — a weak/risky posture that may be an intentional config choice (e.g. auth
  disabled, `origin_validation` off, `/metrics` without a token, plaintext HTTP).
- **SKIP** — the check did not apply (feature disabled, no credentials supplied, or an
  aggressive check without `-aggressive`).
- **ERROR** — the probe itself could not complete (network/parse error).

## Check categories

| Category | What it probes | Maps to |
|----------|----------------|---------|
| `headers` | `X-Content-Type-Options`, `X-Frame-Options`, CSP (`default-src 'self'`, nonce, `frame-ancestors`), **per-request nonce freshness**, `Referrer-Policy`, `Permissions-Policy`, HSTS over TLS, banner disclosure | `securityMiddleware` |
| `auth` | Protected routes return 401 anonymously; forged cookies & bearer tokens rejected; login is POST-only; session cookie `HttpOnly`/`SameSite`/`Secure`; **username-enumeration resistance** | `AuthMiddleware`, `ValidateCredentials` |
| `csrf` | Origin/Referer required on state change; cross-origin POST blocked; CSRF synchronizer token enforced on authenticated sessions | `CSRFMiddleware` |
| `cors` | An arbitrary Origin is **not** reflected; never `ACAO: *` + credentials; `Vary: Origin` present | `corsMiddleware` |
| `traversal` | Byte-level path-traversal payloads (encoded, dot-dot, backslash) over a raw socket leak no files; no directory listing | `handleStatic` |
| `metrics` | `/metrics` bearer token enforced (and wrong token rejected); warns if exposed without a token | `handleMetrics` |
| `ws` | Unauthenticated upgrade rejected; cross-origin upgrade rejected (CSWSH); same-origin upgrade allowed; per-IP connection cap *(aggressive)*; oversized-message read limit *(aggressive)* | `handleWebSocket`, `CheckOrigin`, `SetReadLimit` |
| `input` | `/api/history` bad/inverted/over-long ranges → 400, huge `points` capped; `/api/i18n` rejects junk/traversal language codes | `handleHistory`, `handleI18n` |
| `rate` *(aggressive)* | Login brute-force throttling; Ollama rate limiting | login `RateLimiter`, Ollama limiter |
| `dos` *(aggressive)* | Slowloris / slow-request reaping; oversized request headers rejected; idle-connection-flood resilience | `ReadTimeout`, `MaxHeaderBytes`, `IdleTimeout` |
| `redirect` | No open redirect to a foreign host via crafted paths (`//`, `\`, encoded, base-path tricks) | base-path redirect / CWE-601 |
| `tls` *(https)* | Negotiated TLS version, cipher strength, certificate expiry | reverse-proxy TLS config |
| `bypass` *(aggressive)* | X-Forwarded-For login rate-limit evasion (trust_proxy misconfig) | `getClientIP` / `trust_proxy` |
| `fuzz` *(-fuzz)* | Blind fault injection — see below | the whole surface |

## Safety / `-aggressive`

The default scan is **non-destructive and idempotent**: it reads headers, sends a handful
of single-shot probes, and logs in at most a few times when credentials are supplied.

`-aggressive` adds checks that have **real side effects on the live target** and only run
with the flag (a warning is printed first):

- **`RATE-LOGIN`** — a burst of failed logins to confirm throttling. This **locks out
  login from your IP for ~5 minutes**.
- **`WS-FLOOD`** — opens more than the per-IP WebSocket limit to confirm the cap fires.
- **`WS-MSGBOMB`** — sends an oversized WebSocket message to confirm the read limit.
- **`INPUT-AGG`** — an oversized login body to confirm the `MaxBytesReader` cap.
- **`DOS-SLOWLORIS`** — holds a handful of half-open (slow) requests and verifies the
  server reaps them (and stays responsive). Waits up to `-dos-wait` for the read timeout.
- **`DOS-HEADERBOMB`** — sends a 2 MiB header block to confirm `MaxHeaderBytes` rejects it.
- **`DOS-CONNFLOOD`** — opens many idle connections and verifies the server stays
  responsive and reaps them.
- **`BYPASS-XFF`** — rotates spoofed `X-Forwarded-For` headers against the login limiter
  to detect a `trust_proxy` misconfiguration; consumes login attempts (locks you out).

The `dos` probes wait up to `-dos-wait` (default 35s) for the server to drop a stalled or
idle connection — sized for Kula's default 30s `ReadTimeout`. **Raise `-dos-wait` if the
target runs longer read timeouts**, or the slow-connection checks will report a false
failure. Because each slow/idle probe waits out that window, a `-aggressive` run including
`dos` takes around a minute.

Run `-aggressive` against staging, or accept the temporary lockout/disruption on production.

## Fuzzing / fault injection

`-fuzz` flips kula-scan from "verify known safeguards" to **blind fault injection**: it
throws malformed and extreme input at every surface and watches for things that should
*never* happen. The categories above ask "is defense X present?"; fuzzing asks "what
breaks that nobody thought to defend?" The **anomaly oracle** (black-box, no source
needed) flags:

- **HTTP 5xx** — a handler errored where it should have returned a 4xx.
- **connection reset / EOF** on a valid request — `net/http` recovered a **handler panic**
  and dropped the connection.
- **hang / timeout** — unbounded work or a deadlock.
- **reflected input** — an injected `<canary>` echoed **unescaped** into an HTML/JS
  response (a real XSS sink; escaped reflections in JSON or redirect bodies are ignored).
- **server death** — the final `FUZZ-LIVENESS` probe fails: the instance stopped serving
  after the barrage.

Probes (`-only fuzz`): `FUZZ-QUERY` (history/i18n params), `FUZZ-PATH` (paths + canary),
`FUZZ-BODY` (malformed JSON: truncated, type-confused, deeply-nested, JSON-bomb,
oversized), `FUZZ-METHODS` (verb matrix), `FUZZ-SMUGGLE` (conflicting CL/TE framing),
`FUZZ-WS` (binary/garbage frames + churn), and `FUZZ-LIVENESS` (runs last).

Every probe draws from a **seeded PRNG**. The seed is printed in the report (and the
warning banner); rerun with `-seed <N>` to reproduce a finding exactly, and `-fuzz-iter`
to scale coverage. Fuzzing may create junk sessions and log noise on the target; it is
opt-in via `-fuzz` and independent of `-aggressive` (combine them for the widest sweep).

## How it works

- A `Scanner` (see [`scanner.go`](scanner.go)) holds an `http.Client` with **no cookie jar**
  (cookies are set explicitly per probe) that does **not** follow redirects, so 301/401/403
  responses are observed directly.
- Path-traversal payloads are sent over a **raw TCP/TLS socket** with a verbatim request
  line, bypassing any client-side URL normalization (mirrors the `rawRequest` helper in the
  runtime tests).
- WebSocket probes use `github.com/gorilla/websocket` — the same library the server uses.
- Checks live in [`checks.go`](checks.go), [`checks_ws.go`](checks_ws.go), and
  [`checks_aggressive.go`](checks_aggressive.go); the report/exit logic is in
  [`report.go`](report.go). Classification is unit-tested against secure and insecure mock
  servers in [`checks_test.go`](checks_test.go).
