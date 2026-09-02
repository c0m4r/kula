# Security Model

Security is a core design pillar of Kula, applied in layers: application-level auth and request
hardening, browser-facing headers and integrity, an in-process Landlock sandbox, and a hardened
systemd unit. This page covers the application-level mechanisms; the OS sandbox has its own page
([Landlock Sandbox](09-sandbox.md)).

Most of this lives in [`internal/web/auth.go`](../../internal/web/auth.go) and
[`internal/web/server.go`](../../internal/web/server.go), and is verified both by in-tree tests
and by the out-of-tree [kula-scan](13-kula-scan.md) black-box scanner.

## Authentication

- **Argon2id** password hashing with configurable cost (defaults: memory 32 MB, time 3, threads
  4 — double the OWASP minimum).
- **Multiple users** supported (`web.auth.users`), each with its own hash/salt.
- **Constant-time comparison** (`crypto/subtle.ConstantTimeCompare`) for both username and
  password-hash verification — resists timing-based username enumeration.
- The `hash-password` command reads the password in **raw terminal mode** with asterisk
  masking, never echoing plaintext.

## Sessions

- **Token-only validation** — sessions are *not* bound to client IP or User-Agent (so they
  survive roaming/proxying); validated purely on expiry/validity.
- **SHA-256 token hashing at rest** — plaintext token only on the wire; only its hash is stored
  in `sessions.json` (mode `0600`).
- **Sliding expiration** — a successful request extends the session by `session_timeout`.
- **Absolute lifetime** — a session is rejected and deleted once `now - created_at` exceeds
  `session_max_lifetime` (default 7 days), enforced in `ValidateSession`, `CleanupSessions`
  and on load from disk; the sliding extension is clamped to that deadline. `0` disables it.
- **Live WebSocket re-validation** — auth is enforced at the `/ws` upgrade *and* every 30 s
  for the life of the connection (`wsSessionRecheckInterval`), so an established socket
  cannot outlive its session.
- **Bearer token** accepted in the `Authorization` header.
- A **cleanup goroutine** purges expired sessions every 5 minutes.
- **Cookie flags:** `HttpOnly`, `SameSite=Strict`, and `Secure` (conditional on TLS or trusted
  `X-Forwarded-Proto: https`). With `allowed_origins`, cookies switch to `SameSite=None; Secure`.

## Rate limiting

- **Login:** 5 attempts per 5 minutes, tracked per IP **and** per username.
- **Ollama:** 10 chat requests/IP/minute; 60 metadata requests/IP/minute.

## CSRF protection

- **Origin/Referer validation** on every non-`GET`/`HEAD`/`OPTIONS` request (`ValidateOrigin`).
  Empty Origin headers are rejected (since 0.9.1). Listed `allowed_origins` also pass.
- **Synchronizer token** pattern — a CSRF token delivered to the client and required in the
  `X-CSRF-Token` header on state-changing authenticated requests, validated constant-time.

## CORS

`corsMiddleware` never reflects an arbitrary Origin and never emits `Access-Control-Allow-Origin:
*` together with credentials. It sends `Vary: Origin`, and only echoes origins explicitly listed
in `web.security.allowed_origins`.

## Web security headers

When `web.security.headers` is on (default), responses carry:

- `X-Content-Type-Options: nosniff`
- `X-Frame-Options: DENY` (when `frame_protection`)
- `Content-Security-Policy` with a **fresh random nonce per request**:
  `default-src 'self'; script-src 'self' 'nonce-<rand>'; style-src 'self' 'unsafe-inline';
  frame-ancestors 'none'`
- `Referrer-Policy: strict-origin-when-cross-origin`
- `Permissions-Policy: geolocation=(), microphone=(), camera=()`
- `Strict-Transport-Security` (HSTS) when TLS or trusted `X-Forwarded-Proto: https` is present
  (since 0.15.0)

## Subresource Integrity (SRI)

All served JavaScript carries `integrity="sha384-..."` hashes computed at startup
(`calculateSRIs`, `sha512.Sum384`) and injected into the templated HTML, so a tampered asset
won't execute.

## WebSocket security

- Origin validation on upgrade (CSWSH protection); non-browser clients without an Origin header
  are permitted.
- Global (default 100) and per-IP (default 5) connection limits.
- 4096-byte read limit on incoming messages; 60s read deadline with pong refresh.
- `sync.Once` unregister to prevent double-counting.

## Input validation & limits

- Error responses use `json.Marshal` (not `fmt.Sprintf`) → no JSON injection.
- Body size caps: login body 4096 bytes; Ollama chat 32 KB.
- History caps: ≤31-day window, ≤5000 points.
- Storage path resolved with `filepath.Abs` → directory-traversal resistance.
- Static handler resists byte-level path-traversal payloads (encoded, dot-dot, backslash) and
  serves no directory listings.

## Ollama / AI hardening

- **SSRF prevention:** Ollama URL validated to loopback-only at config load.
- **Prompt sanitization:** null bytes stripped, length clamped to 2000 runes, whitespace
  trimmed.
- **Model-name validation:** `^[A-Za-z0-9._:/-]{1,200}$` — rejects shell metacharacters.
- **Response cap:** 10 MB; **tool loop cap:** 5 rounds.

## Prometheus security

Optional bearer token on `/metrics`, constant-time compared.

## Config & filesystem security

- `KULA_POSTGRES_PASSWORD` is single-quoted and escaped (backslashes and single quotes) to
  prevent libpq connection-string injection (since 0.15.1).
- Storage directory created `0750`; session file `0600`.

## Server timeouts

`ReadTimeout 30s`, `WriteTimeout 60s`, `IdleTimeout 120s`, plus a bounded `MaxHeaderBytes` —
mitigating slowloris and header-bomb DoS. The rightmost (most-trusted) `X-Forwarded-For` IP is
used when `trust_proxy` is set.

## Governance & process

- [`SECURITY.md`](../../SECURITY.md) — private vulnerability reporting policy.
- [`.github/CODE_OF_CONDUCT.md`](../../.github/CODE_OF_CONDUCT.md),
  [`.github/CONTRIBUTING.md`](../../.github/CONTRIBUTING.md).
- CI runs Semgrep ([`.github/workflows/semgrep.yml`](../../.github/workflows/semgrep.yml)) and
  `govulncheck` (via `check.sh`).

## Test coverage

Security-critical paths have dedicated tests:

- `auth_test.go` — hashing determinism, salt generation, credential validation, session
  lifecycle/hashing/legacy-loading, client-IP extraction, Origin validation, CSRF middleware.
- `server_test.go` — template/nonce/CSP injection prevention, SRI verification.
- `runtime_security_test.go` — in-process runtime probes (raw-socket traversal, etc.).
- `websocket_test.go` — connection limits.
- `ollama_test.go` — model validation, prompt sanitization, rate limiting, tool execution.
- `prometheus_test.go` — bearer auth, label escaping.
- `sandbox_test.go` — writes/exec/network outside the allowed set fail.
- `config_test.go` — YAML parsing, env overrides, tier validation.

Plus the external **[kula-scan](13-kula-scan.md)** scanner, which verifies these defenses over
the wire against a *running* instance.

Next: [Landlock Sandbox](09-sandbox.md).
