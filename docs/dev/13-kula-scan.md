# kula-scan

Source: [`cmd/kula-scan/`](../../cmd/kula-scan/) · upstream README:
[`cmd/kula-scan/README.md`](../../cmd/kula-scan/README.md)

`kula-scan` is an **active, black-box safeguard scanner** for a *running* Kula instance. Where
the unit tests, fuzz targets, and `runtime_security_test.go` verify Kula's defenses in the lab,
`kula-scan` verifies them in the field — it points at a live URL and probes it over HTTP/WebSocket
the way a browser or attacker would, then reports per-check whether each safeguard actually holds
in the deployed configuration (your proxy, your TLS, your base path, your config).

It imports **nothing** from Kula's `internal/` packages — every assertion is made over the wire —
so it complements the in-tree tests rather than duplicating them. It is **not** built into the
release binary or Docker image, but lives in the same module, so `go vet ./...` and
`go test ./...` cover it.

## Build & run

```bash
go build -o kula-scan ./cmd/kula-scan
./kula-scan http://localhost:27960
```

## Usage

```
kula-scan [flags] <target-url>
```

| Flag | Meaning |
|------|---------|
| `-username`, `-password` | Credentials to unlock authenticated checks (login, CSRF token, WS auth) |
| `-base-path` | Base path if Kula is mounted under one (auto-detected from the URL path) |
| `-timeout` | Per-request timeout (default `10s`) |
| `-insecure` | Skip TLS verification (self-signed test instances) |
| `-aggressive` | Enable disruptive checks (real side effects — see below) |
| `-fuzz` | Enable blind fault-injection fuzzing |
| `-fuzz-iter` | Iterations per randomized fuzz probe (default 200) |
| `-seed` | PRNG seed (0 = random; the chosen seed is reported for reproducibility) |
| `-only` | Comma-separated categories to run |
| `-fail-on` | Min FAIL severity forcing non-zero exit: `info\|low\|medium\|high\|critical` (default `high`) |
| `-json` | Emit findings as JSON |
| `-no-color` | Disable ANSI colors |
| `-v` | Verbose: print each request/response |

### Exit status

- `0` — no failing safeguard at/above `-fail-on`.
- `1` — one or more FAIL at/above `-fail-on` (IDs printed).
- `2` — usage error (bad flag, unreachable target).

This makes it usable as a release/CI gate: stand up the instance, scan it, fail the build if a
safeguard regressed.

### Statuses

`PASS` (present & correct) · `FAIL` (missing/bypassable — a real finding) · `WARN` (weak posture,
possibly intentional, e.g. auth off) · `SKIP` (not applicable) · `ERROR` (probe couldn't complete).

## Check categories

| Category | Probes | Maps to |
|----------|--------|---------|
| `headers` | `X-Content-Type-Options`, `X-Frame-Options`, CSP + per-request nonce freshness, `Referrer-Policy`, `Permissions-Policy`, HSTS over TLS, banner disclosure | `securityMiddleware` |
| `auth` | Protected routes 401 anonymously; forged cookies/bearer rejected; login POST-only; cookie flags; username-enumeration resistance | `AuthMiddleware`, `ValidateCredentials` |
| `csrf` | Origin/Referer required; cross-origin POST blocked; synchronizer token enforced | `CSRFMiddleware` |
| `cors` | Arbitrary Origin not reflected; never `ACAO:*`+credentials; `Vary: Origin` | `corsMiddleware` |
| `traversal` | Byte-level path-traversal over a raw socket leaks nothing; no directory listing | `handleStatic` |
| `metrics` | `/metrics` bearer enforced; warns if exposed without a token | `handleMetrics` |
| `ws` | Unauth/cross-origin upgrade rejected; same-origin allowed; per-IP cap & read limit (aggressive) | `handleWebSocket` |
| `input` | `/api/history` bad/inverted/out-of-retention over-long ranges → 400, huge `points` capped; `/api/i18n` junk codes rejected | `handleHistory`, `handleI18n` |
| `rate` *(aggressive)* | Login brute-force throttling; Ollama rate limiting | rate limiters |
| `dos` *(aggressive)* | Slowloris reaping; oversized headers rejected; idle-flood resilience | `ReadTimeout`, `MaxHeaderBytes`, `IdleTimeout` |
| `redirect` | No open redirect to a foreign host via crafted paths | base-path redirect / CWE-601 |
| `tls` *(https)* | Negotiated TLS version, cipher strength, cert expiry | reverse-proxy TLS |
| `bypass` *(aggressive)* | X-Forwarded-For login rate-limit evasion (trust_proxy misconfig) | `getClientIP` / `trust_proxy` |
| `fuzz` *(-fuzz)* | Blind fault injection (anomaly oracle) | the whole surface |

## Safety

The default scan is **non-destructive and idempotent**. `-aggressive` adds checks with **real
side effects** (each warned first):

- `RATE-LOGIN` / `BYPASS-XFF` — burst failed logins; **locks out your IP for ~5 minutes**.
- `WS-FLOOD` / `WS-MSGBOMB` — exceed the per-IP WS cap / send an oversized frame.
- `INPUT-AGG` — oversized login body.
- `DOS-SLOWLORIS` / `DOS-HEADERBOMB` / `DOS-CONNFLOOD` — slow/idle/header-bomb DoS resilience.
  These wait up to `-dos-wait` (default 35s, sized for the 30s `ReadTimeout`) — raise it if your
  target uses longer timeouts, or they'll false-fail.

Run `-aggressive` against staging, or accept temporary disruption on production.

## Fuzzing (`-fuzz`)

Blind fault injection at every surface, with an **anomaly oracle** that flags HTTP 5xx (handler
errored), connection reset/EOF (recovered panic), hang/timeout, **unescaped reflected canary**
(XSS sink), and **server death** (`FUZZ-LIVENESS` runs last). Probes: `FUZZ-QUERY`, `FUZZ-PATH`,
`FUZZ-BODY`, `FUZZ-METHODS`, `FUZZ-SMUGGLE`, `FUZZ-WS`. Every probe draws from a seeded PRNG; the
seed is printed so any finding is reproducible with `-seed <N>`.

## Implementation

- `Scanner` ([scanner.go](../../cmd/kula-scan/scanner.go)) holds an `http.Client` with **no
  cookie jar** that does **not** follow redirects, so 301/401/403 are observed directly.
- Traversal payloads are sent over a **raw TCP/TLS socket** to bypass client-side URL
  normalization.
- WebSocket probes use `gorilla/websocket` (the same lib the server uses).
- Checks: [`checks.go`](../../cmd/kula-scan/checks.go),
  [`checks_ws.go`](../../cmd/kula-scan/checks_ws.go),
  [`checks_aggressive.go`](../../cmd/kula-scan/checks_aggressive.go),
  [`checks_dos.go`](../../cmd/kula-scan/checks_dos.go),
  [`checks_tls.go`](../../cmd/kula-scan/checks_tls.go),
  [`checks_bypass.go`](../../cmd/kula-scan/checks_bypass.go),
  [`checks_fuzz.go`](../../cmd/kula-scan/checks_fuzz.go); report/exit in
  [`report.go`](../../cmd/kula-scan/report.go); classification unit-tested in
  [`checks_test.go`](../../cmd/kula-scan/checks_test.go).

Next: [Adding a Metric Type](14-adding-metrics.md).
