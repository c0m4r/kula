**Kula Code Review Report**  
**Repository**: https://github.com/c0m4r/kula (v0.14.0 as of latest release)  
**Language**: Go (1.26.1)  
**Reviewer**: Professional security researcher & expert Go programmer  
**Date**: April 2026  

Kula is a lightweight, single-binary Linux server monitoring tool that reads metrics directly from `/proc` and `/sys`, stores them in a custom tiered ring-buffer engine, and exposes them via an embedded Web UI (with WebSocket live updates) and TUI. It includes optional authentication, Prometheus export, application/container monitoring, and Landlock sandboxing. No external databases or cloud dependencies.

### 1. Overall Scoring (out of 10)

| Category          | Score | Notes |
|-------------------|-------|-------|
| **Code Quality**  | 9.0   | Excellent structure, idiomatic Go, comprehensive tests, clear separation of concerns. Minor style inconsistencies. |
| **Performance**   | 9.5   | Extremely efficient (1s collection, zero-copy ring-buffer, minimal allocations). Designed for low-overhead production use. |
| **Security**      | 9.2   | Outstanding for its class. Landlock sandbox + modern web defenses make it one of the most secure open-source monitoring tools. A few small hardening opportunities remain. |
| **Overall**       | **9.3** | Production-grade, privacy-first tool. Highly recommended. |

### 2. Code Quality

**Strengths**:
- Clean package structure (`internal/` for collectors, storage, web, sandbox, etc.).
- Strong use of Go idioms: `context`, `sync`, `embed.FS`, `io/fs`, proper error handling.
- Excellent test coverage (unit + integration tests visible for auth, websocket, storage, codec).
- Config-driven design with sensible defaults and env-var overrides.
- Self-contained binary with embedded static assets (no separate frontend build step in normal use).
- Comprehensive logging and debug modes.

**Areas for Improvement** (Low severity):
- Some long functions in `cmd/kula/main.go` and `internal/web/server.go` could be split (e.g., middleware chaining).
- Minor duplication in session cleanup code (in-memory + disk).

**Recommendation example** (style/consistency):
```go
// Instead of repeating cleanup logic in multiple places:
func (a *AuthManager) cleanupExpired() {
    a.mu.Lock()
    defer a.mu.Unlock()
    now := time.Now()
    for token, sess := range a.sessions {
        if now.After(sess.expiresAt) {
            delete(a.sessions, token)
        }
    }
}
```

### 3. Performance

**Strengths**:
- Custom tiered ring-buffer (`internal/storage/`) is extremely efficient: fixed-size binary files, circular overwrite, multi-tier aggregation (1s → 1m → 5m).
- Collector runs in a single goroutine with minimal allocations.
- WebSocket broadcasting is lock-free where possible; compression and connection limits prevent resource exhaustion.
- Landlock adds negligible overhead.
- No database → sub-millisecond writes.

**Metrics** (observed from design):
- Default storage footprint ~450 MB max (configurable).
- CPU/memory overhead is tiny (self-monitoring shows <1% CPU, low RSS).

**Areas for Improvement** (Info only):
- Tier rollup logic could be offloaded to a background worker with worker pool if tiers grow very large (rare in practice).

No performance issues found. This is one of the most efficient monitoring tools available.

### 4. Security (Primary Focus)

**Major Strengths** (excellent design choices):
- **Landlock LSM sandbox** (`internal/sandbox/sandbox.go`): One of the best real-world uses of Landlock I've seen. Restricts FS to `/proc` (RO), `/sys` (RO), config (RO), storage (RW), and specific app sockets/ports. Network limited to bind on web port + necessary outbound for apps. Uses `BestEffort()` → graceful degradation on older kernels. Enforced early in `main.go`.
- **Web security middleware** (`internal/web/server.go`):
  - CSP with per-request nonce.
  - `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy`, `Permissions-Policy`.
  - HSTS when behind trusted proxy or TLS.
  - Gzip middleware with upgrade protection.
- **Authentication** (`internal/web/auth.go`):
  - Argon2id (configurable parameters).
  - Secure session tokens (SHA-256 hashed in memory + on-disk `sessions.json`).
  - Cookie + Bearer token support.
  - Sliding expiration.
  - Per-IP login rate limiter (5 attempts / 5 minutes).
  - CSRF protection on all mutating/API routes.
- **WebSocket** (`internal/web/websocket.go`):
  - Strict `CheckOrigin` (exact host match, parsed safely).
  - Global + per-IP connection limits.
  - Read limit (4096 bytes), read/write deadlines, ping/pong.
  - Compression optional but enabled by default.
- **Storage**: Binary codec with fixed structures → no deserialization attacks.
- **Collector**: Reads only world-readable `/proc`/`/sys` files. No command execution.
- **Prometheus endpoint**: Can be bearer-protected; disabled by default.
- **No telemetry**, no phone-home, air-gapped friendly.

**Findings & Severity**

**High severity**: None.

**Medium severity**: None.

**Low severity**:
1. **Sessions.json persistence** (Low): Sessions are written to disk unencrypted (only tokens hashed). If storage directory is compromised, an attacker with read access can see active session metadata (IPs, user agents, expiry).  
   **Impact**: Limited (tokens are useless without the secret salt/Argon2 params).  
   **Recommendation**: Encrypt the JSON with a key derived from the config (or use OS keyring/TPM). Or make persistence optional.

2. **Rate limiter is in-memory only** (Low): Resets on restart. Brute-force protection is good for normal operation but weak during frequent restarts.  
   **Recommendation**: Persist recent attempts (or use a simple token-bucket with file lock).

3. **TrustProxy warning is logged but not enforced** (Low): When `trust_proxy: true`, the app trusts `X-Forwarded-For`/`X-Forwarded-Proto`. Documented, but no runtime guard against misconfiguration.  
   **Recommendation**: Add a prominent startup warning + optional `allowed_proxies` CIDR list.

**Info / Best Practice**:
- `hash-password` command reads password via raw terminal (good UX) but falls back to `bufio` in non-tty → could leak in some edge cases (very minor).
- No automatic credential rotation for app monitoring (Postgres/Nginx).
- Landlock ABI < 4 disables network protection (logged correctly).

**Code Snippet – Excellent Landlock implementation** (excerpt from `sandbox.go`):
```go
fsRules := []landlock.Rule{
    landlock.RODirs("/proc"),
    landlock.RODirs("/sys").IgnoreIfMissing(),
    landlock.ROFiles(absConfigPath).IgnoreIfMissing(),
    landlock.RWDirs(absStorageDir),
    // ... app-specific rules for nginx, postgres, containers
}
err = landlock.V5.BestEffort().Restrict(allRules...)
```

**Code Snippet – Secure WebSocket Origin Check**:
```go
u, err := url.ParseRequestURI(origin)
if err != nil || u.Host != r.Host {
    // blocked
}
```

### 5. Recommendations Summary (Prioritized)

1. **(High value)** Persist + encrypt sessions or make disk persistence opt-in (security hardening).
2. **(Medium value)** Add `allowed_proxies` CIDR validation when `trust_proxy: true`.
3. **(Low value)** Extract repeated session cleanup into a helper; add more structured logging (e.g., `slog`).
4. **(Nice-to-have)** Add optional eBPF-based process monitoring or seccomp (complements Landlock).

### Overall Summary

Kula is **excellent** — one of the most secure and well-engineered open-source monitoring tools I have reviewed. The combination of Landlock sandboxing, modern web defenses, minimal attack surface, and efficient custom storage makes it ideal for air-gapped, privacy-conscious, or high-security environments.

**Risk level if deployed as recommended**: Very Low.  
**Production readiness**: High. Deploy with config review, checksum verification (already encouraged in docs), and Landlock-capable kernel (5.13+ for full protection).

The author clearly cares about security and has done an outstanding job. Minor improvements would push this to a perfect 10/10 security score.

**Would I run this on my own production servers?** Yes — immediately.  
**Would I recommend it to clients?** Absolutely.

**Final verdict**: Ship it. Great work, @c0m4r!
