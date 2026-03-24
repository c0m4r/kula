# Kula Security Review

**Reviewer:** Claude Sonnet 4.6  
**Date:** 2026-03-23  
**Scope:** Full codebase — backend (Go), frontend (JavaScript/HTML), configuration  

---

## Executive Summary

Kula demonstrates a **strong security posture** for a self-hosted monitoring dashboard. Core security controls — authentication, session management, CSRF protection, HTTP security headers, and input validation — are all correctly implemented. Notably, the application includes a **Linux Landlock sandbox** that restricts filesystem and network access at the kernel level, which significantly limits the blast radius of any theoretical exploitation.

Two medium-severity findings were identified: a missing HSTS header and an unescaped field in a CSP `style-src` context. All remaining issues are low-severity or informational design notes.

---

## Findings

### Medium

#### M1 — Missing HSTS Header

**File:** `internal/web/server.go`, security middleware (~line 175)
**Description:** The `securityMiddleware` sets `X-Frame-Options`, `X-Content-Type-Options`, `Content-Security-Policy`, `Referrer-Policy`, and `Permissions-Policy`, but does not include `Strict-Transport-Security`. Without HSTS, browsers will not enforce HTTPS connections on subsequent visits, leaving the application vulnerable to SSL stripping attacks on networks where an attacker controls traffic.

**Current headers set:**
```go
w.Header().Set("X-Content-Type-Options", "nosniff")
w.Header().Set("X-Frame-Options", "DENY")
w.Header().Set("Content-Security-Policy", ...)
w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
// HSTS absent
```

**Recommendation:** Conditionally add HSTS when serving over TLS or behind a trusted HTTPS proxy:
```go
if r.TLS != nil || (s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https") {
    w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
}
```

---

#### M2 — CSP Missing `style-src` Directive

**File:** `internal/web/server.go`, line 177
**Description:** The Content Security Policy is:
```
default-src 'self'; script-src 'self' 'nonce-<nonce>'; frame-ancestors 'none';
```
`style-src` is not explicitly set, so it falls back to `default-src 'self'`, which blocks inline styles. However, Chart.js and the dashboard UI apply inline styles extensively via JavaScript (e.g., `card.style.opacity`, `card.style.order`, etc.). If a browser enforces the `default-src` fallback strictly for `style-src`, these inline style manipulations will be blocked silently.

More importantly, since `style-src` is not locked to `'nonce-...'` or `'strict-dynamic'`, any CSS injection via a future vulnerability could load external stylesheets.

**Recommendation:** Add an explicit `style-src 'self' 'unsafe-inline'` directive (inline styles are ubiquitous in the UI and cannot be easily refactored) or audit all inline style usages and migrate to CSS classes.

---

### Low

#### L1 — WebSocket Accepts Connections Without Origin Header

**File:** `internal/web/websocket.go`, lines 26–29
**Description:** The `CheckOrigin` function explicitly allows WebSocket upgrades when no `Origin` header is present:
```go
if origin == "" {
    // Explicitly allow non-browser clients (like CLI tools) which omit the Origin
    return true
}
```
Browsers always send `Origin` on WebSocket connections, so JavaScript-based CSWSH is blocked. However, custom tooling (scripts, API consumers) without an `Origin` header bypasses this check. For a monitoring dashboard exposing sensitive system metrics, this is an acceptable design tradeoff, but it should be documented.

**Note:** The `SameSite: Strict` cookie and `X-CSRF-Token` requirement provide compensating controls for browser-based attacks.

**Recommendation:** Document the design intent. If the threat model excludes non-browser clients, consider optionally enforcing Origin even for API use via a config flag.

---

#### L2 — Language Parameter Uses Blacklist Validation

**File:** `internal/web/server.go`, lines 614–618
**Description:** The `/api/i18n` endpoint validates the `lang` query parameter using a blacklist:
```go
if strings.Contains(lang, "..") || strings.Contains(lang, "/") || strings.Contains(lang, "\\") {
    jsonError(w, "invalid language", http.StatusBadRequest)
    return
}
```
A whitelist of the 26 supported ISO codes would be more robust. The secondary defense (`i18n.GetRawLocale` reads from Go's `embed.FS` which has no OS-level path traversal) prevents actual exploitation, but defence-in-depth favors a whitelist.

**Recommendation:**
```go
validLangs := map[string]bool{"ar": true, "bn": true, "cs": true, /* ... */}
if !validLangs[lang] {
    jsonError(w, "invalid language", http.StatusBadRequest)
    return
}
```

---

#### L3 — `innerHTML` Usage With Partially Unescaped Fields

**File:** `internal/web/static/js/app/alerts.js`, lines 89–96
**Description:** Alert items are rendered via `innerHTML`. `title` and `detail` are escaped with `escapeHTML()`, but `a.icon` is inserted directly:
```javascript
list.innerHTML = state.alerts.map(a => `
    <div class="alert-item">
        <span class="alert-icon">${a.icon}</span>  // not escaped
        ...
        <div class="alert-item-title">${escapeHTML(a.title)}</div>
```
Currently `icon` is hardcoded to emoji literals inside `alerts.js` and no user-supplied data flows to it, so there is no exploitable XSS path. However, it violates defence-in-depth — if alert generation logic ever changes to include user-controlled data, this becomes an XSS sink.

**Recommendation:** Apply `escapeHTML()` to `a.icon` as a matter of principle.

---

### Informational

#### I1 — Sessions Not Bound to IP or User-Agent

**File:** `internal/web/auth.go`, struct `sessionData` (lines 48–55)
**Description:** The `sessionData` struct stores `IP` and `UserAgent` fields, but `ValidateSession()` does not use them for validation. Sessions are portable across IP changes and browser updates. This is a usability tradeoff (e.g., mobile users on changing networks) rather than a vulnerability, but stolen session tokens would be fully usable from any IP.

**Note:** The 24-hour session timeout and `SameSite: Strict` + `HttpOnly` cookie flags reduce the practical risk significantly.

---

#### I2 — Rate Limiter IP Entry Accumulation

**File:** `internal/web/auth.go`, `RateLimiter` struct
**Description:** The `RateLimiter` stores per-IP attempt timestamps in a `sync.Map`. Cleanup runs every 5 minutes. An attacker cycling through a large CIDR range could accumulate many entries in memory between cleanups, marginally increasing memory pressure. This is a common tradeoff in in-process rate limiters.

**Note:** For public-facing deployments, consider using a dedicated reverse proxy with rate limiting (e.g., nginx `limit_req`) to handle this at the network boundary.

---

#### I3 — CSRF Token Stored in `sessions.json`

**File:** `internal/web/auth.go`, `sessionData.CSRFToken` (line 51)
**Description:** CSRF tokens are persisted to `sessions.json` alongside hashed session tokens. The file has `0600` permissions. This is correct, but worth noting: CSRF token persistence means tokens survive server restarts. Some implementations prefer ephemeral CSRF tokens (regenerated per restart) for a narrower validity window.

**Not a vulnerability** given the file permission, but documenting for awareness.

---

## Strengths

The following security controls are correctly and robustly implemented.

### Authentication & Session Management

| Control | Implementation | Notes |
|---------|---------------|-------|
| Password hashing | Argon2id, 64MB RAM, 4 threads | Resistant to GPU/ASIC attacks |
| Constant-time comparison | `subtle.ConstantTimeCompare` | Prevents timing oracle on credentials |
| Session token generation | 32 bytes `crypto/rand` | 256-bit entropy |
| Session token storage | SHA-256 hashed in `sessions.json` | Not stored plaintext |
| Session file permissions | `0600` | Owner read/write only |
| Login rate limiting | 5 attempts / 5 min per IP | Prevents brute force |
| Session timeout | Configurable, default 24h | Sliding window with cleanup |

### CSRF Protection

Dual-layer defence is in place for all state-modifying requests:
1. **Origin/Referer header validation** (`auth.go`, `ValidateOrigin`) — rejects requests where the Origin or Referer does not match the Host header.
2. **Synchronizer token** (`auth.go`, `CSRFMiddleware`) — requires `X-CSRF-Token` header to match the per-session token, validated with constant-time comparison.

### HTTP Cookies

```
HttpOnly: true        — no JS access to session cookie
Secure:   true        — HTTPS only (when TLS or trusted proxy)
SameSite: Strict      — no cross-site inclusion
```

### HTTP Security Headers

| Header | Value |
|--------|-------|
| `Content-Security-Policy` | `default-src 'self'; script-src 'self' 'nonce-<per-request>'; frame-ancestors 'none'` |
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `DENY` |
| `Referrer-Policy` | `strict-origin-when-cross-origin` |
| `Permissions-Policy` | `geolocation=(), microphone=(), camera=()` |

CSP uses a **per-request nonce** for scripts, preventing injection of unauthorized scripts even if a future XSS sink were found.

### Subresource Integrity

**File:** `internal/web/server.go`, SRI hash computation (~line 707)
SHA-384 SRI hashes are computed at startup for all JavaScript modules and injected into `<link rel="modulepreload">` tags. This prevents tampering with static assets in transit or on disk.

### WebSocket Security

- Origin header validated against request Host (prevents CSWSH from browsers)
- Global connection limit: 100 (configurable)
- Per-IP connection limit: 5 (configurable)
- Message read limit: 4096 bytes (`SetReadLimit`)
- Read deadlines enforced
- Ping/pong keepalive to detect stale connections

### Input Validation

- API time range parameters: RFC3339 format, max 31-day window, `to >= from` enforced
- Request body size: capped at 4096 bytes for login endpoint
- `points` parameter: capped at 5000
- Prometheus label values: escaped for `\`, `"`, newlines

### Landlock Sandbox (Linux)

**File:** `internal/sandbox/sandbox.go`
The application implements **Linux Landlock** filesystem and network sandboxing, applied at startup after config and storage initialization. On supported kernels (5.13+):

| Resource | Access |
|----------|--------|
| `/proc`, `/sys` | Read-only |
| Config file | Read-only |
| Storage directory | Read-write |
| Network | TCP bind on configured port only |

On kernels without Landlock support the application degrades gracefully with a logged warning. This is a notable defence-in-depth measure: even if a vulnerability were exploited for code execution, the process cannot read arbitrary filesystem paths, write outside the storage directory, or make outbound network connections.

### HTTP Server Timeouts

```
ReadTimeout:  30s
WriteTimeout: 60s
IdleTimeout:  120s
```

### Static File Security

All static assets are served from Go's `embed.FS` (compiled into the binary), eliminating path traversal vulnerabilities for static file serving. Directory listings are explicitly blocked.

---

## Summary Table

| Finding | Severity | File | Recommendation |
|---------|----------|------|----------------|
| M1 — Missing HSTS | Medium | `server.go:175` | Add `Strict-Transport-Security` on TLS/proxy |
| M2 — CSP missing `style-src` | Medium | `server.go:177` | Add explicit `style-src 'self' 'unsafe-inline'` |
| L1 — WS accepts no-Origin | Low | `websocket.go:26` | Document intent; consider config flag |
| L2 — Language param blacklist | Low | `server.go:614` | Switch to explicit allowlist |
| L3 — `alert.icon` not escaped | Low | `alerts.js:90` | Apply `escapeHTML()` to all fields |
| I1 — No IP/UA session binding | Info | `auth.go:48` | Design tradeoff, acceptable |
| I2 — Rate limiter accumulation | Info | `auth.go` | Mitigated by periodic cleanup |
| I3 — CSRF token persisted | Info | `auth.go:51` | No action required |

---

## Recommendations by Priority

1. **Add HSTS header** (M1) — one-line fix, high impact for HTTPS deployments.
2. **Add explicit `style-src` to CSP** (M2) — prevents ambiguity; inline styles are pervasive in Chart.js usage.
3. **Escape `alert.icon`** (L3) — trivial change, improves defence-in-depth.
4. **Whitelist language codes** (L2) — already safe via embedded FS, but cleaner validation.
5. **Document WebSocket empty-Origin behaviour** (L1) — make the security tradeoff explicit in code or README.
