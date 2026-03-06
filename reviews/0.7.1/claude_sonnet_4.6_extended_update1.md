# Kula — Delta Security & Code Review
## Review of Fixes Against Prior Report Findings

**Diff reviewed:** Post-0.7.1 unreleased changes (CHANGELOG: Unreleased)  
**Prior report date:** March 2026  
**Delta review date:** March 2026  
**Files changed:** `auth.go`, `server.go`, `config.go`, `config.example.yaml`, all collector `*.go`, `app.js`, `index.html`, `style.css`, `game.*`, test suites + testdata

---

## Summary Scorecard

| Category | Before | After | Delta |
|---|---|---|---|
| Code Quality | 7.0 / 10 | 7.8 / 10 | **+0.8** ✅ |
| Performance | 7.5 / 10 | 7.4 / 10 | **−0.1** ⚠️ |
| Security | 6.0 / 10 | 7.0 / 10 | **+1.0** ✅ |
| Documentation | 8.5 / 10 | 8.8 / 10 | **+0.3** ✅ |
| Maintainability | 7.0 / 10 | 7.8 / 10 | **+0.8** ✅ |
| **Overall** | **7.2 / 10** | **7.8 / 10** | **+0.6** ✅ |

---

## Finding-by-Finding Status

### 🔴 Critical / High Priority

| ID | Finding | Status | Notes |
|---|---|---|---|
| SEC-01 | Auth default-off | ✅ **PARTIALLY FIXED** | Default bind changed to `127.0.0.1` — good mitigation |
| SEC-02 | No native TLS | ❌ **NOT ADDRESSED** | Still HTTP-only; reverse proxy still required |
| SEC-04 | No rate limiting on login | ✅ **ALREADY FIXED** | `RateLimiter` was pre-existing; `getClientIP` now centralised |
| Build | Unit tests missing | ✅ **FIXED** | Comprehensive test suite added across all collectors |

### 🟡 Medium Priority

| ID | Finding | Status | Notes |
|---|---|---|---|
| SEC-03 | Session cookie flags | ✅ **FIXED** | `trust_proxy` guard added for `Secure` flag; `SameSite=Strict` confirmed |
| SEC-05 | Argon2id parameters | ✅ **FIXED** | Now configurable; OWASP reference added to docs |
| SEC-06 | HTTP security headers | ✅ **PRE-EXISTING** | `securityMiddleware` already present (not new in this diff) |
| SEC-07 | CSRF protection | ❌ **NOT ADDRESSED** | No CSRF tokens introduced |
| SEC-08 | WebSocket origin validation | ✅ **FIXED** | Listed in CHANGELOG; not shown in diff (prior commit) |
| SEC-10 | Docker runs as root | ❌ **NOT ADDRESSED** | No Dockerfile changes |
| CQ-01 | JSON codec for storage | ❌ **NOT ADDRESSED** | Ring-buffer still uses JSON encoding |
| CQ-04 | Session store in-memory only | ✅ **FIXED (with caveats)** | Sessions now persisted to `sessions.json` — see new findings |
| PF-01 | Collection timeout watchdog | ❌ **NOT ADDRESSED** | No timeout on collector loop |
| PF-02 | WebSocket unbounded write buffers | ❌ **NOT ADDRESSED** | No drop policy added |

### 🟢 Low Priority

| ID | Finding | Status | Notes |
|---|---|---|---|
| SEC-09 | Information disclosure via errors | ❌ **NOT ADDRESSED** | |
| CQ-02 | Error handling in /proc parsers | ➖ **PARTIALLY** | Paths now injectable; error handling itself unchanged |
| CQ-03 | Interface abstraction for testability | ✅ **FIXED (pragmatic)** | Package-level vars + testdata, not interfaces, but effective |
| CQ-05 | Monolithic app.js | ❌ **NOT ADDRESSED** | `fetchConfig()` refactored out, but file remains monolithic |
| CQ-06 | Structured logging | ❌ **NOT ADDRESSED** | |
| CQ-07 | Shell scripts `set -euo pipefail` | ❌ **NOT ADDRESSED** | |
| PF-03 | Tier aggregation re-read I/O | ❌ **NOT ADDRESSED** | |
| PF-04 | Chart.js from CDN / no SRI | ❌ **NOT ADDRESSED** | |

---

## Detailed Analysis of Changes

### ✅ WIN: Default Bind Address Changed to Localhost (SEC-01)

```yaml
# Before:
listen: "0.0.0.0"
# After:
listen: "127.0.0.1"
```

This is the single most impactful change in the diff. A freshly deployed Kula instance no longer exposes the dashboard to the network by default. Good pragmatic fix — it achieves the intent of the original recommendation without forcing authentication on.

**Remaining gap:** Auth is still `enabled: false` by default. An operator who deliberately changes the bind address to `0.0.0.0` without enabling auth is back to the same risk as before. Consider adding a startup warning log when `listen != "127.0.0.1"` and `auth.enabled == false`.

---

### ✅ WIN: Configurable Argon2 Parameters (SEC-05)

The `Argon2Config` struct and its plumbing through `config.go`, `auth.go`, and `main.go` are well-executed. The OWASP reference in `config.example.yaml` is a good touch. The `hash-password` command now reads config first and prints the parameters used alongside the hash output — excellent UX.

**One remaining concern:** The default `time: 1, memory: 65536` sits in the acceptable OWASP range for memory-heavy configurations, but `time: 1` is the absolute minimum. The comment in `config.example.yaml` says `time: 2-4, memory: 65536-262144` is recommended, but the default value below that comment is `time: 1`. This creates a subtle inconsistency — the recommendation and the default contradict each other. The default should match the floor of the recommended range (`time: 2`).

---

### ✅ WIN: Collector Testability via Injectable Paths (CQ-03)

```go
var (
    procPath = "/proc"
    sysPath  = "/sys"
    runPath  = "/run"
)
```

This is a clean, pragmatic solution. The package-level vars are overridden in each test:

```go
func TestParseProcStat(t *testing.T) {
    procPath = "testdata/proc"
    ...
}
```

The testdata fixtures are thorough and realistic — they cover cpu, memory, network, disk, system, self, and process collectors. The tests will now catch regressions when `/proc` format edge cases arise across kernel versions.

**Minor concern:** Package-level mutable state is not goroutine-safe if tests ever run in parallel (`t.Parallel()`). Since `procPath` is a global var, concurrent test execution would cause data races. The tests don't currently use `t.Parallel()`, so this is fine for now, but it's a latent trap. Adding a `-race` flag to the CI check command would surface this immediately if parallelism is ever added.

---

### ✅ WIN: Session Persistence Across Restarts (CQ-04)

Sessions are now saved to `sessions.json` on graceful shutdown and loaded at startup. The implementation correctly:
- Skips expired sessions during load.
- Filters expired sessions during save.
- Uses `0600` file permissions.
- Handles the "no file yet" case gracefully.

However, there are several issues with this implementation that introduce **new risks**:

---

### 🚨 NEW FINDING — NF-01: Session Tokens Stored in Plaintext JSON
**Severity:** `HIGH` | **File:** `internal/web/auth.go` — `SaveSessions()`

```go
toSave = append(toSave, sessionData{
    Token:     token,  // ← valid session token written to disk in plaintext
    Username:  sess.username,
    IP:        sess.ip,
    ...
})
```

`sessions.json` contains live, valid session tokens in plaintext. Anyone with read access to the storage directory — including any process running on the same machine as the same or a more privileged user — can extract these tokens and hijack active sessions. This is especially dangerous because the storage directory also contains the ring-buffer metric data and likely has broader read permissions than a dedicated secrets store.

**Recommendation:** Store only a hashed form of the token on disk (e.g., `SHA-256(token)`). On load, rebuild the in-memory map keyed by the hash. On validation, compare `SHA-256(presented_token)` against the map. This way, a stolen `sessions.json` yields no usable tokens.

```go
import "crypto/sha256"

func hashToken(token string) string {
    h := sha256.Sum256([]byte(token))
    return hex.EncodeToString(h[:])
}
```

---

### 🚨 NEW FINDING — NF-02: `getClientIP` Always Trusts `X-Forwarded-For`
**Severity:** `HIGH` | **File:** `internal/web/server.go` — `getClientIP()`

```go
func getClientIP(r *http.Request) string {
    ip := r.Header.Get("X-Forwarded-For")
    if ip != "" {
        return ip  // ← trusted unconditionally
    }
    host, _, err := net.SplitHostPort(r.RemoteAddr)
    ...
}
```

This function is used for **rate limiting** in `handleLogin` and for **session fingerprinting** in `ValidateSession`. Because `X-Forwarded-For` is trusted unconditionally — regardless of the `trust_proxy` config flag — an attacker can:

1. **Bypass the rate limiter** by rotating the `X-Forwarded-For` header on each request (e.g., `X-Forwarded-For: 1.2.3.4`, then `X-Forwarded-For: 1.2.3.5`, etc.).
2. **Potentially forge session ownership** if they know a victim's IP/UA, by crafting the header accordingly.

This directly undermines the `trust_proxy` fix applied to the `Secure` cookie flag — that fix is correct, but it was not extended to `getClientIP`. The fix created an asymmetry: the cookie respects `trust_proxy`, but the rate limiter and session binding do not.

**Recommendation:** Gate `X-Forwarded-For` trust on `trust_proxy` config in `getClientIP`, or pass the config into the function:

```go
func getClientIP(r *http.Request, trustProxy bool) string {
    if trustProxy {
        if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
            // Take only the first (leftmost) IP from the chain
            return strings.TrimSpace(strings.SplitN(xff, ",", 2)[0])
        }
    }
    host, _, err := net.SplitHostPort(r.RemoteAddr)
    if err != nil {
        return r.RemoteAddr
    }
    return host
}
```

Also note: `X-Forwarded-For` can be a comma-separated list (e.g., `1.2.3.4, 5.6.7.8`). The current implementation returns the entire raw string, not just the client IP, which would cause all rate-limit and session-binding lookups to operate on a multi-IP string — effectively making them always miss.

---

### ⚠️ NEW FINDING — NF-03: Session Persistence Only on Graceful Shutdown
**Severity:** `LOW-MEDIUM` | **File:** `internal/web/server.go`

```go
func (s *Server) Shutdown(ctx context.Context) error {
    if err := s.auth.SaveSessions(); err != nil { ... }
    ...
}
```

Sessions are only persisted at graceful shutdown. If the process is killed with `SIGKILL`, crashes due to a panic, or is terminated by the OOM killer, the session file is not updated and all sessions since the last graceful restart are lost — defeating the purpose of the feature.

**Recommendation:** Add a periodic background goroutine that saves sessions every 60 seconds (configurable). This also amortises the write cost and limits session loss to at most one save interval on an ungraceful exit.

---

### ⚠️ NEW FINDING — NF-04: Non-Atomic Session File Write
**Severity:** `LOW-MEDIUM` | **File:** `internal/web/auth.go` — `SaveSessions()`

```go
return os.WriteFile(path, data, 0600)
```

`os.WriteFile` overwrites the file in-place. If the process is killed mid-write (e.g., disk-full, SIGKILL, power loss), the result is a truncated or partially-written `sessions.json`. On next startup, `LoadSessions` will fail to `json.Unmarshal` the corrupt file, but then returns an error — which `Start()` only logs as a warning and continues. In this case all sessions are silently dropped, which is safe but silent.

**Recommendation:** Use an atomic write pattern (write to temp file, then `os.Rename`) to guarantee the file is either fully written or unchanged:

```go
tmp := path + ".tmp"
if err := os.WriteFile(tmp, data, 0600); err != nil {
    return err
}
return os.Rename(tmp, path)
```

---

### ⚠️ NEW FINDING — NF-05: Session Binding to IP+UserAgent — Usability vs. Security Trade-off
**Severity:** `LOW` | **File:** `internal/web/auth.go` — `ValidateSession()`

```go
if sess.ip != ip || sess.userAgent != userAgent {
    return false
}
```

Binding sessions to IP and User-Agent is a defence-in-depth measure but has two weaknesses:

1. **UserAgent is trivially spoofable** — any attacker who has stolen a session token can also observe and copy the UserAgent string (e.g., from a browser extension, shared log file, or network capture). It provides minimal additional security.
2. **IP binding breaks legitimate use cases** — users on mobile networks, behind NAT with address rotation, using VPNs, or accessing through a load balancer with multiple egress IPs will find their sessions silently invalidated mid-work with no explanation.

This change also has a **performance regression**: `ValidateSession` was upgraded from `sync.RWMutex.RLock()` to `sync.Mutex.Lock()` to support the sliding expiration update — meaning every single request that validates a session now acquires an exclusive write lock, blocking all concurrent requests from validating simultaneously. For the typical single-user scenario this is unnoticeable, but it's worth noting.

**Recommendation:** Keep IP binding but make it configurable (`auth.bind_session_to_ip: false` by default, or only when `trust_proxy: true`). Consider dropping the UserAgent binding entirely — it provides marginal security for real cost in session stability.

---

### ✅ WIN: `trust_proxy` for Secure Cookie (SEC-03)

```go
// Before (trusted blindly):
Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",

// After (gated on config):
Secure: r.TLS != nil || (s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https"),
```

This is exactly the right fix. The same pattern is correctly applied to both `handleLogin` and `handleLogout`. The config option is well-named, documented, and defaults to `false`. ✅

---

### ✅ WIN: Proper Logout Implementation

The `handleLogout` endpoint, `RevokeSession`, and frontend `handleLogout` are all well-implemented. The server-side revocation + cookie deletion + frontend state wipe is correct. The logout button is hidden/shown based on `auth_required` state from `/api/auth/status`, which is the right signal. ✅

---

### ✅ WIN: `fetchConfig()` Refactored into Named Function (CQ-05 — Partial)

The config fetch logic was properly extracted from `init()` into a dedicated `fetchConfig()` function and is now called post-login and post-auth-check. This is a meaningful improvement to `app.js` maintainability. The `init()` function is now meaningfully leaner. The overall file remains monolithic, but this is a step in the right direction.

---

### 🔎 OBSERVATION: `api/logout` Route Protection Gap

```go
mux.Handle("/api/login", loggedApiMux)
mux.Handle("/api/logout", loggedApiMux)       // ← not behind auth middleware
mux.Handle("/api/", s.auth.AuthMiddleware(loggedApiMux))
```

`/api/logout` is explicitly exempted from the `AuthMiddleware`. This is intentional (you need to be able to hit logout even with a bad session), but it means anyone — authenticated or not — can POST to `/api/logout`. An attacker who knows a valid session cookie value (e.g., from a log) cannot benefit from this directly (revocation only helps the defender), and unauthenticated callers simply find no cookie to revoke. So this is functionally safe, but it does mean logout is a potential DoS vector if the RevokeSession path were to ever become expensive. For the current implementation this is not a concern.

---

### 🔎 OBSERVATION: Test Data `proc/stat` Format — Missing `intr`, `ctxt`, `btime` Lines

The `testdata/proc/stat` fixture only contains `cpu` and `cpu0`/`cpu1` lines:

```
cpu  2000 0 1000 50000 200 100 50 0 0 0
cpu0 1000 0 500 25000 100 50 25 0 0 0
cpu1 1000 0 500 25000 100 50 25 0 0 0
```

Real `/proc/stat` files also contain `intr`, `softirq`, `ctxt`, `btime`, `processes`, `procs_running`, and `procs_blocked` lines. If the parser makes assumptions about line ordering or expects these lines, the test fixture will silently under-cover those code paths. This is not a bug in the current implementation (parsers that skip non-cpu lines will work fine), but it's a documentation gap in the test coverage.

---

## Remaining Open Findings from Prior Report

These were identified in the original report and **remain unaddressed**:

| ID | Finding | Severity | Effort to Fix |
|---|---|---|---|
| SEC-02 | No native TLS support | HIGH | Medium |
| SEC-07 | No CSRF protection on state-changing endpoints | MEDIUM | Medium |
| SEC-09 | Verbose HTTP error responses | LOW-MEDIUM | Low |
| SEC-10 | Docker container runs as root | MEDIUM | Low |
| CQ-01 | JSON codec for ring-buffer storage | MEDIUM | High |
| CQ-05 | Monolithic `app.js` | LOW | Medium |
| CQ-06 | Structured logging (`log/slog`) | LOW | Low |
| CQ-07 | Shell scripts missing `set -euo pipefail` | LOW | Low |
| PF-01 | No collection timeout watchdog | MEDIUM | Low |
| PF-02 | Unbounded WebSocket write buffers | MEDIUM | Low |
| PF-03 | Tier aggregation reads from disk unnecessarily | LOW | Medium |
| PF-04 | Chart.js from CDN without SRI hash | LOW | Low |

---

## New Findings Summary

| ID | Finding | Severity | File |
|---|---|---|---|
| NF-01 | Session tokens stored in plaintext in `sessions.json` | HIGH | `auth.go` |
| NF-02 | `getClientIP` always trusts `X-Forwarded-For` regardless of `trust_proxy` | HIGH | `server.go` |
| NF-03 | Sessions only persisted on graceful shutdown | LOW-MEDIUM | `server.go` |
| NF-04 | Non-atomic `sessions.json` write (corruption risk) | LOW-MEDIUM | `auth.go` |
| NF-05 | IP+UA session binding — usability risk + write-lock regression | LOW | `auth.go` |

---

## Conclusion

This is a solid round of fixes. The headline wins are the **localhost-only default bind** (the most impactful single change), **configurable Argon2 parameters**, the **`trust_proxy` guard on the Secure cookie**, and the **comprehensive collector unit test suite** — which was the most-requested reliability improvement.

The session persistence feature is well-intentioned and the basic flow is correct, but it introduces **two new HIGH-severity issues** that should be addressed before the next release: the plaintext token storage in `sessions.json` (NF-01) is a straightforward fix using token hashing, and the unconditional `X-Forwarded-For` trust in `getClientIP` (NF-02) undermines both rate limiting and session binding when the server is exposed directly to untrusted networks.

The `time: 1` default in Argon2 config while the comment recommends `time: 2-4` is a small but confusing inconsistency that should be corrected. TLS (SEC-02) remains the most significant unaddressed structural gap for any deployment outside a localhost or trusted-LAN context.

---

*Delta review based on the provided `changes.diff` against the prior report findings. New findings (NF-01 through NF-05) are net-new issues introduced by or discovered during this review of the patch set.*
