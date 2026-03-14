# Review of c0m4r/kula

**Scope:** I scanned the repository’s main server, auth, WebSocket, storage, sandbox, and frontend files (notably `internal/web/*`, `internal/storage/*`, `internal/config/*`, `cmd/*`, and the embedded static JS/HTML). Key files inspected include `internal/web/server.go`, `internal/web/auth.go`, `internal/web/websocket.go`, `internal/storage/*.go`, `internal/config/config.go`, `cmd/kula/main.go`, and the embedded frontend `internal/web/static/*` (e.g. `app.js`, `index.html`). Evidence for findings is cited inline. ([GitHub][1])

---

# TL;DR — overall verdict

* **Security posture:** **Good** for a self-contained local monitoring tool. The project uses Argon2id for password hashing, session tokens with server-side validation, Landlock sandboxing (best-effort), origin checks for CSRF, and reasonable WebSocket origin checks. Still, several **high/medium** issues should be fixed (cookie Secure semantics behind proxies, CSP/connect-src vs websockets, some CSRF/Origin edge-cases, rate-limiter robustness, and a few minor info-leaks). ([GitHub][2])
* **Performance posture:** **Strong**; the tiered ring-buffer design and buffered reads/writes look efficient and conservative about disk I/O. Some tuning/guardrails are recommended for large histories and memory spikes. ([GitHub][3])
* **Code quality:** **High** — idiomatic Go, clear organization, locking discipline, and defensive checks throughout. A few places could improve logging granularity, error wrapping, and comment clarity. ([GitHub][1])

I scored each axis below (0–10):

* **Security:** 7.5 / 10
* **Performance:** 8.5 / 10
* **Code quality & maintainability:** 8 / 10

(Scoring rationale: baseline safety features present, robust storage, but some hardening and small bugs remain.)

---

# Summary of major findings (with severity & recommendation)

> Each finding below includes (A) short description, (B) why it matters (risk), (C) evidence (file/citation), and (D) recommended fix.

---

## 1) Session cookie *Secure* semantics and reverse proxy usage — **High**

**Why:** Cookie is set with `Secure` only when `r.TLS != nil || (trustProxy && X-Forwarded-Proto == "https")`. If users terminate TLS at a reverse proxy but forget to enable `trust_proxy`, cookies may be sent over plain HTTP (risk: session disclosure). ([GitHub][1])

**Evidence:** `handleLogin` uses `Secure: r.TLS != nil || (s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https")`. ([GitHub][1])

**Recommendation:**

* Document `trust_proxy` prominently in README and warn users strongly that it must be set when terminating TLS at a proxy (and that X-Forwarded-Proto must be trusted only from a trusted proxy).
* Prefer **explicit** config flag for “always set Secure on cookies” (safe default) or allow operator to explicitly indicate TLS termination. Example change:

```go
secureFlag := r.TLS != nil || s.cfg.ForceSecureCookies || (s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https")
http.SetCookie(w, &http.Cookie{ Name: "kula_session", Value: token, Path: "/", HttpOnly: true, Secure: secureFlag, SameSite: http.SameSiteStrictMode, })
```

* Add startup-time check that warns if `cfg.Auth.Enabled && !cfg.TrustProxy && listen` is not a TLS listener and port 80 is open.

**Severity:** High

---

## 2) CSRF / Origin validation: strict but brittle — **Medium**

**Why:** The CSRF defense uses Origin/Referer header equality (`ValidateOrigin`), and the CSRFMiddleware blocks requests that lack Origin/Referer. Some legitimate non-browser clients or reverse-proxied requests may lack Origin and will be rejected; conversely, if proxies rewrite Host/X-Forwarded-Host incorrectly, origin checks can be bypassed/misapplied. ([GitHub][2])

**Evidence:** `CSRFMiddleware` rejects state-modifying requests unless `ValidateOrigin(r)` returns true; `ValidateOrigin` returns `false` if neither Origin nor Referer are present. ([GitHub][2])

**Recommendation:**

* Keep Origin checks as defense-in-depth, but allow **authenticated** non-browser clients with other protections (e.g. require Authorization Bearer token for API clients, or use SameSite cookies + CSRF token if browser access expected).
* Accept empty Origin only for requests with explicit `Authorization` header or known client certificate. Alternatively, add a configurable `AllowNoOriginForClients` option with documentation.
* Log rejected requests with origin detail to help operators debug blocked clients.

**Severity:** Medium

---

## 3) CSP & embedded scripts vs WebSocket `connect-src` — **Medium**

**Why:** `securityMiddleware` sets a CSP that includes `script-src 'self' 'nonce-...'`, but it does not set `connect-src` to allow websocket endpoints explicitly. Some browsers require `connect-src` to permit `ws:`/`wss:` connections; omission could break functionality in strict environments. Additionally, earlier versions included `connect-src` but current code uses only `default-src` and `script-src`. ([GitHub][1])

**Evidence:** CSP header: `default-src 'self'; script-src 'self' 'nonce-%s'; frame-ancestors 'none';` (no explicit connect-src). The server uses `/ws` WebSocket endpoint. ([GitHub][1])

**Recommendation:**

* Add `connect-src 'self' ws: wss:` to CSP (or `connect-src 'self' wss:` if only secure websockets expected). Example:

```go
w.Header().Set("Content-Security-Policy", fmt.Sprintf(
  "default-src 'self'; connect-src 'self' ws: wss:; script-src 'self' 'nonce-%s'; frame-ancestors 'none';", nonce))
```

* Test with deployments behind TLS proxies (use `wss:`).

**Severity:** Medium

---

## 4) WebSocket Origin handling & non-browser clients — **Low → Medium depending on deployment**

**Why:** `Upgrader.CheckOrigin` allows non-browser clients that omit Origin. This is reasonable, but it means CLI tools can open WebSockets without Origin auth. The code also compares the origin host to `r.Host` (good). Operators should be aware of the trade-off. ([GitHub][4])

**Evidence:** Upgrader `CheckOrigin` returns `true` when `Origin == ""` (explicitly to allow CLI clients). Otherwise it requires `u.Host == r.Host`. ([GitHub][4])

**Recommendation:**

* Document the behavior; allow an operator flag to *require* origin header for all connections if they wish (tighten by default).
* Consider rate-limiting or authentication for WebSocket endpoints (already limited by global and per-IP connection caps, but ensure caps are configured). ([GitHub][4])

**Severity:** Low–Medium (config-dependent)

---

## 5) Rate limiter is simple and may be susceptible to IP spoofing or lack of per-account lockout — **Medium**

**Why:** The rate limiter keys by client IP only. If auth is enabled and a reverse proxy forwards many client IPs (or an attacker spoofs), brute-force protections can be ineffective. Also the limiter increments the attempt count regardless of whether the username matched or not, which is fine, but there is no account-specific or global backoff beyond "5 in 5 minutes". ([GitHub][2])

**Evidence:** `RateLimiter.Allow(ip)` keeps a slice of timestamps per IP and allows up to 5 attempts in 5 minutes. ([GitHub][2])

**Recommendation:**

* Combine IP-based limiting with per-username throttling and/or exponential backoff.
* Use constant-time response timings for failed logins (already comparing constant-time) and ensure error messages do not reveal whether username exists.
* Consider adding lockout or CAPTCHA integration for public deployments (document risk if running internet-exposed).

**Severity:** Medium

---

## 6) Session binding to IP + User-Agent may cause usability issues — **Low**

**Why:** Sessions are bound to exact `ip` and `userAgent` (string equality). Mobile networks or some proxies may change client IP on session refresh and break sessions. From security perspective this is stricter (good), but may impact legitimate users. ([GitHub][2])

**Recommendation:**

* Consider relaxing binding to a network / ASN range, or allow configurable options: bind to user agent + partial IP (first /24) or drop IP check behind `trust_proxy` careful config. Log mismatches for admin visibility.

**Severity:** Low (mostly usability tradeoff)

---

## 7) Sessions persisted to disk as hashed tokens — **Good / Low-risk but note**

**Why:** Sessions are stored as hashed token keys on disk (`sessions.json` stores the hashed token). That means an attacker getting `sessions.json` cannot directly use stored values without preimage (SHA-256 preimage of session token). However, the hashed token is the map key and will be used as-is by LoadSessions. This is a reasonable design. ([GitHub][2])

**Recommendation:** Continue to protect the storage directory (default `/var/lib/kula`) with strict FS perms; ensure sessions file has mode `0600` (code currently writes with `0600`). Consider optionally encrypting the sessions file with a key if operators request it.

**Severity:** Low

---

## 8) Storage engine (ring buffer) — robust but watch for disk/IO tuning — **Low**

**Why:** The tiered ring-buffer implementation appears solid, with headers, sentinel handling, buffered reads and pre-filtering by timestamp, and careful concurrency. This should be performant and stable for the intended scope. Watch out for the default Max sizes & knobs on small disks (but `parseSize` and checks exist). ([GitHub][3])

**Recommendation:**

* Ensure operators set storage directory on a reliable disk; add a small alert if free space gets low (the tool already uses fixed-size files, but logging would help).
* Consider periodic `fsync` semantics for critical write durability (currently header updates are every 10 writes; acceptable but configurable).

**Severity:** Low

---

## 9) Landlock sandboxing — good defense-in-depth but kernel dependent — **Low**

**Why:** Landlock is used with `BestEffort()` so Kula degrades gracefully on kernels that do not support all features. This is a strong, modern hardening mechanism; but operators must know it’s kernel dependent. ([GitHub][5])

**Recommendation:**

* Document supported kernel versions and note which sandbox rules are effective on which kernels. Consider a runtime check that prints the effective sandbox rules at startup for operator inspection.

**Severity:** Low

---

## 10) Frontend: embedded SPA, XSS protections — Good but watch template data sources — **Low**

**Why:** Templates are rendered server-side with a nonce and SRI for JS assets. The frontend includes `escapeHTML()` where relevant. The UI does not appear to render user-provided HTML. ([GitHub][1])

**Recommendation:**

* Keep escaping any user-supplied strings returned in APIs. The API JSON encoding uses `json.NewEncoder()` — good. Avoid `template.HTML` or unsafe injections in templates.
* Add CSP `connect-src` (see above) so WebSocket usage works under restrictive CSP.

**Severity:** Low

---

# Other quality / performance notes

* **Good locking discipline** in storage and server code (mutexes used, R/W locks where appropriate). ([GitHub][3])
* **HTTP timeouts** are set (`ReadTimeout`, `WriteTimeout`, `IdleTimeout`) — good for resilience. ([GitHub][1])
* **WebSocket pump** uses ping/pong and set deadlines, and limits read size — good. ([GitHub][4])
* **JSON payload size caps** for login (`MaxBytesReader` 4 KiB) are set — good. ([GitHub][1])

---

# Suggested roadmap (concrete, prioritized)

1. **(High)** Add safer default for cookie Secure handling and a clear startup-time warning if TLS termination is ambiguous; add `ForceSecureCookies` flag. (See finding #1.) ([GitHub][1])
2. **(Medium)** Adjust CSP to include `connect-src 'self' ws: wss:` and validate behavior across browsers; add tests. (Finding #3.) ([GitHub][1])
3. **(Medium)** Improve/extend rate-limiting: per-account throttling and exponential backoff. (Finding #5.) ([GitHub][2])
4. **(Medium)** Make CSRFMiddleware less brittle: allow authenticated non-browser clients (with Authorization) or allow operator to configure expected client types. (Finding #2.) ([GitHub][2])
5. **(Low)** Add an admin endpoint that prints effective sandboxing rules and warns about Landlock support levels. (Finding #9.) ([GitHub][5])
6. **(Low)** Add configurable `fsync` policy and/or make header flush frequency configurable for the storage tier for deployments that require stricter durability. (Storage notes.) ([GitHub][3])

---

# Concrete code pointers (quick diffs / snippets)

**CSP connect-src fix (server.go, securityMiddleware):**

```go
w.Header().Set("Content-Security-Policy",
  fmt.Sprintf("default-src 'self'; connect-src 'self' ws: wss:; script-src 'self' 'nonce-%s'; frame-ancestors 'none';", nonce))
```

(Place right where `securityMiddleware` builds CSP.) ([GitHub][1])

**Cookie Secure flag (handleLogin):**
Add `ForceSecureCookies` to `WebConfig` (bool) and compute:

```go
secureFlag := r.TLS != nil || s.cfg.ForceSecureCookies || (s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https")
```

Then use `Secure: secureFlag` when setting the cookie. ([GitHub][1])

---

# What I scanned & what I did not (transparency)

I examined the following key files (selected, but representative):

* `internal/web/server.go` (routing, CSP, handlers) — evidence: turn8view0. ([GitHub][1])
* `internal/web/auth.go` (Argon2 hashing, sessions, CSRF checks) — evidence: turn9view0. ([GitHub][2])
* `internal/web/websocket.go` (WebSocket upgrade, pumps, origin check) — evidence: turn10view0. ([GitHub][4])
* `internal/storage/tier.go` and `internal/storage/store.go` (ring-buffer format, reads/writes, locking) — evidence: turn13view0 & turn12view0. ([GitHub][3])
* `internal/config/config.go` and `config.example.yaml` (defaults and parsing) — evidence: turn7view0 & turn7view1. ([GitHub][6])
* `internal/web/static/app.js` and `internal/web/static/index.html` (frontend, escaping, web UI) — evidence: turn4view1 & turn4view0. ([GitHub][7])
* `internal/sandbox/sandbox.go` (Landlock usage) — evidence: turn14view0. ([GitHub][5])

I did *not* do:

* A full line-by-line audit of every single `.go` file in the repo (there are many subpackages and helpers), or dynamic fuzzing / runtime tests. I focused on the security-sensitive and high-impact areas (auth, session handling, WebSocket, storage, sandbox, and the embedded frontend), which are the places where vulnerabilities typically appear for this kind of app. If you want, I can expand to exhaustively scan every `.go` and `.js` in the repo and produce a file-by-file checklist (I tried to prioritize the most relevant files already).

---

# Final recommendation & overall summary

`:contentReference[oaicite:40]{index=40}` is **well-written** and uses many modern best-practices:

* strong password hashing (Argon2id),
* server-side session validation with hashed tokens persisted safely,
* Landlock sandboxing,
* efficient tiered ring-buffer storage,
* careful WebSocket handling (deadlines, size limits, ping/pong), and
* CSP + SRI for embedded frontend assets.

The **highest-priority** fixes are operational (cookie Secure semantics behind TLS proxies, CSP `connect-src`, and making CSRF origin rules flexible for legitimate non-browser clients). Once those are addressed (plus tightening rate-limiting), the project will be in very strong shape for safe deployment on local networks and even small public deployments behind a properly configured reverse proxy.

If you want, I can:

* produce a **patch** (PR-style diff) implementing the cookie Secure default + CSP connect-src change and an improved CSRFMiddleware fallback; and/or
* run a deeper per-file static checklist for every `.go`/`.js`/`.html` file and produce a file-by-file scorecard.

Which would you like me to do next? (I can generate concrete diffs and a checklist immediately.)

[1]: https://raw.githubusercontent.com/c0m4r/kula/main/internal/web/server.go "raw.githubusercontent.com"
[2]: https://raw.githubusercontent.com/c0m4r/kula/main/internal/web/auth.go "raw.githubusercontent.com"
[3]: https://raw.githubusercontent.com/c0m4r/kula/main/internal/storage/tier.go "raw.githubusercontent.com"
[4]: https://raw.githubusercontent.com/c0m4r/kula/main/internal/web/websocket.go "raw.githubusercontent.com"
[5]: https://raw.githubusercontent.com/c0m4r/kula/main/internal/sandbox/sandbox.go "raw.githubusercontent.com"
[6]: https://raw.githubusercontent.com/c0m4r/kula/main/internal/config/config.go "raw.githubusercontent.com"
[7]: https://raw.githubusercontent.com/c0m4r/kula/main/internal/web/static/app.js "raw.githubusercontent.com"
