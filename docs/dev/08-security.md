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
- **No sessions without auth** — with `web.auth.enabled: false`, `/api/login` answers `404`
  and `ValidateCredentials` accepts nothing, so no session can be minted. Sessions are not
  loaded either, and the empty set saved at shutdown clears `sessions.json`.
- **Bound to the credential** — each session records a fingerprint of the account it was
  issued for (SHA-256 over username and Argon2id hash; the hash is keyed by the salt, so
  the salt itself stays out). A session read from `sessions.json` is dropped on load unless
  that fingerprint matches the config: a removed
  account, a changed password or salt, or a session saved by a version before 0.22.0 (which
  also minted sessions with auth disabled) does not come back.
- **SHA-256 token hashing at rest** — plaintext token only on the wire; only its hash is stored
  in `sessions.json` (mode `0600`).
- **Sliding expiration** — a successful request extends the session by `session_timeout`.
- **Absolute lifetime** — a session is rejected and deleted once `now - created_at` exceeds
  `session_max_lifetime` (default 7 days), enforced in `ValidateSession`, `CleanupSessions`
  and on load from disk; the sliding extension is clamped to that deadline. `0` disables it.
- **Live WebSocket re-validation** — auth is enforced at the `/ws` upgrade *and* every 30 s
  for the life of the connection (`wsSessionRecheckInterval`), so an established socket
  cannot outlive its session.
- **Bearer token** accepted in the `Authorization` header wherever the cookie is
  (`authenticatedSessionToken`, cookie first): protected routes, `/ws`, `/api/auth/status`;
  `/api/logout` revokes both. CSRF tokens are checked only for cookie sessions.
- A **cleanup goroutine** purges expired sessions every 5 minutes.
- **Cookie flags:** `HttpOnly`, `SameSite=Strict`, and `Secure` (conditional on TLS or trusted
  `X-Forwarded-Proto: https`). With `allowed_origins`, cookies switch to `SameSite=None; Secure`.

## Rate limiting

- **Login:** 15 attempts per 5 minutes per IP, and 5 per IP+username pair (the per-account
  counter is keyed on the pair, so one attacker cannot lock out an account for everyone).
  An IPv6 client is counted by its /64 (`loginClientKey`): one host routinely has the whole
  /64, and counted per address it had a fresh budget from each, and could fill the IP
  limiter's 16,384-key table (which refuses new addresses when full) with malformed requests
  that never reach the password hash. An address in the NAT64 well-known prefix `64:ff9b::/96`
  counts as the IPv4 address it carries, or every IPv4 client of a translator would share one
  /64.
- **Password hashing:** a process-wide semaphore (`hashSlots`) bounds concurrent Argon2id
  computations to a quarter of `GOMAXPROCS`, between 1 and 4, and to as many as fit in 128 MiB
  of `argon2.memory` (at least one). Every attempt hashes, unknown
  usernames included, so the per-IP limits alone would let many addresses keep hundreds of
  `argon2.memory`-sized hashes in flight. A login that finds no free slot within 2 s gets
  `503` with `Retry-After: 1`, and its attempt is refunded to both login limiters
  (`RateLimiter.Refund`): no password was checked.
- **Ollama:** 10 chat requests/IP/minute; 60 metadata requests/IP/minute.

## CSRF protection

- **Origin/Referer validation** on every non-`GET`/`HEAD`/`OPTIONS` request (`ValidateOrigin`,
  when `origin_validation` is on). Empty Origin headers are rejected (since 0.9.1). Listed
  `allowed_origins` also pass.
- **Synchronizer token** pattern — a CSRF token delivered to the client and required in the
  `X-CSRF-Token` header on state-changing authenticated requests, validated constant-time.

## CORS

`corsMiddleware` never reflects an arbitrary Origin and never emits `Access-Control-Allow-Origin:
*` together with credentials. It sends `Vary: Origin`, and only echoes origins explicitly listed
in `web.security.allowed_origins`.

## DNS rebinding

With auth disabled, a page on another site can point its own domain at the address Kula
listens on. The browser then treats Kula as that site, so the page's script can read the
dashboard, the API and `/ws`, and the Origin check passes as well (Origin and `Host` name the
same site). The request still carries the attacker's name in `Host`. `hostCheckMiddleware`
([`host_check.go`](../../internal/web/host_check.go)) therefore answers, while auth is
disabled on a TCP listener, only names a public DNS server cannot answer for: IP addresses,
`localhost`, single-label names,
`.local`/`.lan`/`.home`/`.corp`/`.localdomain`/`.internal`/`.test`/`.home.arpa` names, the
machine's hostname (`os.Hostname`) and `global.hostname`, each matched exactly, the hosts of
`http`/`https` `allowed_origins` (an app or extension origin names no server), and
`web.security.allowed_hosts` (normalized at load; `"*"` disables the check). Anything else gets
a JSON `403`, logged once per name for up to 64 names. It wraps every route inside the base
path except `/health`, `/status` and a `/metrics` that asks for a bearer token (a rebinding page
cannot supply it). With auth enabled the page has no session cookie to send, so only a
`/metrics` without a token, the one route with data and no credential of its own, is checked. A
Unix socket has no browser connecting directly and is never checked.

The check assumes the resolver is not the attacker's. A local resolver answers the local-only
and single-label names, so one an attacker runs (a hostile Wi-Fi network's DHCP-provided DNS)
can rebind those too; the protection there is auth, not the `Host` check.

## Web security headers

When `web.security.headers` is on (default), responses carry:

- `X-Content-Type-Options: nosniff`
- `X-Frame-Options: DENY` (when `frame_protection`)
- `Content-Security-Policy` with a **fresh random nonce per request**:
  `default-src 'self'; script-src 'self' 'nonce-<rand>'; style-src 'self'; object-src 'none';
  base-uri 'self'; form-action 'self';`
  plus `frame-ancestors 'none'` when `frame_protection`, and on the game page alone
  `connect-src 'self' <origin>` when `game_score_url` is configured (`handleGame` replaces the
  policy; `contentSecurityPolicy` builds both). `base-uri` and `form-action` do not fall back to
  `default-src`: an injected `<base>` could otherwise move the nonce'd scripts' relative URLs
  to another origin, and the `web.base_path` `<base>` is same-origin. Styles come only from the
  served stylesheets: no inline
  `style` attribute, `<style>` element or `setAttribute('style', …)` works, so the frontend
  sets styles through the CSSOM (`element.style.x = …`), which the policy allows. The Chromium
  fixtures (`watchCSPViolations` in `chromium_test_helper.mjs`) fail on any violation.
- `Referrer-Policy: strict-origin-when-cross-origin`
- `Permissions-Policy: geolocation=(), microphone=(), camera=()`
- `Strict-Transport-Security` (HSTS) when TLS or trusted `X-Forwarded-Proto: https` is present
  (since 0.15.0)

## Subresource Integrity (SRI)

Every script a page loads carries an `integrity="sha384-..."` hash computed at startup
(`calculateSRIs`, `sha512.Sum384`) and injected into the templated HTML, so a tampered asset
won't execute. The hashes cover the bytes actually served — the minified form when
`web.minify_assets` is on.

A browser checks a hash only where the page names the script. Classic scripts and the entry
module carry it on their `<script>` tag; a module that is imported, directly or through another
module, is checked only through a `<link rel="modulepreload" integrity=...>` in the page head,
and without one it is fetched and run unchecked. `TestPagesLoadEveryScriptWithIntegrity`
renders every page template, walks the import graph from its entry modules, and fails on any
script or imported module without the hash of its served bytes.

## WebSocket security

- Origin validation on upgrade (CSWSH protection); non-browser clients without an Origin header
  are permitted.
- Global (default 100) and per-IP (default 5) connection limits.
- 4096-byte read limit on incoming messages; 60s read deadline with pong refresh.
- `sync.Once` unregister to prevent double-counting.

## Input validation & limits

- Error responses use `json.Marshal` (not `fmt.Sprintf`) → no JSON injection.
- Body size caps: login body 4096 bytes; Ollama chat 32 KB.
- History caps: ≤5000 output points and ≤31 days for every requested interval.
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
  prevent libpq connection-string injection (since 0.16.0).
- Storage directory created `0750`; session file `0600`.
- Packages and the guided installer install `/etc/kula` as `root:kula 0750` and seed
  `config.yaml` as `root:kula 0640` (the snap: `0600` root), and set those modes again on
  upgrade: the secrets in it reach root and the `kula` group only, and the daemon cannot rewrite
  its own config. OpenRC and the packages keep `/var/lib/kula` at `0750`.

## Server timeouts

`ReadTimeout 30s`, `WriteTimeout 60s`, `IdleTimeout 120s`, plus Go's default 1 MiB
`MaxHeaderBytes` — mitigating slowloris and header-bomb DoS. The rightmost (most-trusted)
`X-Forwarded-For` address of the header's last line is used when `trust_proxy` is set; an entry
that is not an address counts as the proxy's own, so made-up values share one limiter key.

## Governance & process

- [`SECURITY.md`](../../SECURITY.md) — private vulnerability reporting policy.
- [`.github/CODE_OF_CONDUCT.md`](../../.github/CODE_OF_CONDUCT.md),
  [`.github/CONTRIBUTING.md`](../../.github/CONTRIBUTING.md).
- CI runs Semgrep ([`.github/workflows/semgrep.yml`](../../.github/workflows/semgrep.yml)) and
  `govulncheck` (via `check.sh`).
- [zizmor](https://docs.zizmor.sh/)
  ([`.github/workflows/zizmor.yml`](../../.github/workflows/zizmor.yml)) audits the workflows
  themselves and fails when one leaves its token's permissions at the default, lets checkout
  persist credentials, expands untrusted input into a script, or uses an action that is not an
  `actions/*` one pinned to a full commit SHA. Its scan step is the only one in the workflows
  that is handed the `GITHUB_TOKEN` — the read-only `contents: read` one — because the audits
  that check the pinned actions against the GitHub API (impostor commits, known-vulnerable
  versions, a version comment that does not match its SHA) need an authenticated client.
  `addons/ci-local.sh` has no token and runs the offline audits only.

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
- `host_check_test.go` — DNS rebinding: refused and answered `Host` names, when the check
  applies, bounded logging.
- `config_test.go` — YAML parsing, env overrides, tier validation, `allowed_origins` and
  `allowed_hosts` normalization.

Plus the external **[kula-scan](13-kula-scan.md)** scanner, which verifies these defenses over
the wire against a *running* instance.

Next: [Landlock Sandbox](09-sandbox.md).
