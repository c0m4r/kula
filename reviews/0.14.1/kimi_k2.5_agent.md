# Kula Code Review Report

**Repository:** https://github.com/c0m4r/kula  
**Project:** Kula - Lightweight Linux Server Monitoring Tool  
**Language:** Go  
**Review Date:** April 2026  
**Version Analyzed:** v0.14.0 (main branch, commit ee57d5b)

---

## Executive Summary

Kula is a well-architected Go application for Linux server monitoring with a modern web UI, TUI interface, and Prometheus metrics export. The codebase demonstrates good security awareness with features like Landlock sandboxing, Argon2 password hashing, CSRF protection, and rate limiting. However, several security improvements, code quality enhancements, and performance optimizations are recommended.

### Overall Scores

| Category | Score | Grade |
|----------|-------|-------|
| **Security** | 78/100 | B+ |
| **Code Quality** | 82/100 | B+ |
| **Performance** | 75/100 | B |
| **Documentation** | 85/100 | B+ |
| **Overall** | **80/100** | **B+** |

---

## 1. Security Analysis

### 1.1 High Severity Issues

#### SEC-H1: PostgreSQL Password Stored in Plain Text in Configuration
**File:** `internal/config/config.go:174`  
**Severity:** HIGH

The PostgreSQL configuration stores the database password in plain text:

```go
type PostgresConfig struct {
    // ...
    Password string `yaml:"password"`
    // ...
}
```

**Risk:** Configuration files may be world-readable or committed to version control, exposing credentials.

**Recommendation:**
- Support environment variable override for sensitive credentials
- Consider using a secrets manager or encrypted configuration
- At minimum, document the security risk in configuration examples

```go
// Recommended improvement
type PostgresConfig struct {
    Password string `yaml:"password"`
    PasswordEnv string `yaml:"password_env"` // e.g., "KULA_POSTGRES_PASSWORD"
}

func (p *PostgresConfig) GetPassword() string {
    if p.PasswordEnv != "" {
        if envPass := os.Getenv(p.PasswordEnv); envPass != "" {
            return envPass
        }
    }
    return p.Password
}
```

---

#### SEC-H2: Session File Permissions May Be Insufficient
**File:** `internal/web/auth.go:317`  
**Severity:** HIGH

```go
return os.WriteFile(path, data, 0600)
```

While `0600` permissions are used, there's no verification that the parent directory has secure permissions. If the storage directory is world-readable, session files could be accessed by other users.

**Recommendation:**
```go
func (a *AuthManager) SaveSessions() error {
    // Ensure parent directory has secure permissions
    if err := os.MkdirAll(a.storageDir, 0700); err != nil {
        return fmt.Errorf("creating storage directory: %w", err)
    }
    
    // Verify directory permissions
    if info, err := os.Stat(a.storageDir); err == nil {
        mode := info.Mode().Perm()
        if mode&0077 != 0 {
            log.Printf("WARNING: Storage directory %s has overly permissive permissions: %04o", 
                a.storageDir, mode)
        }
    }
    
    // Write with restrictive permissions
    path := filepath.Join(a.storageDir, "sessions.json")
    return os.WriteFile(path, data, 0600)
}
```

---

#### SEC-H3: Potential Integer Overflow in Port Parsing
**File:** `internal/config/config.go:285-291`  
**Severity:** HIGH

```go
if port64, err := strconv.ParseInt(portStr, 10, 32); err == nil {
    port := int(port64)
    if port > 0 && port <= 65535 {
        cfg.Web.Port = port
    }
}
```

The code correctly validates the port range, but the conversion from `int64` to `int` could overflow on 32-bit architectures where `int` is 32 bits but the upper bits could be problematic.

**Recommendation:**
```go
if port64, err := strconv.ParseInt(portStr, 10, 32); err == nil {
    if port64 > 0 && port64 <= 65535 {
        cfg.Web.Port = int(port64) // Safe: range validated before conversion
    }
}
```

---

### 1.2 Medium Severity Issues

#### SEC-M1: WebSocket Origin Check Can Be Bypassed in Certain Configurations
**File:** `internal/web/websocket.go:24-47`  
**Severity:** MEDIUM

The `CheckOrigin` function allows non-browser clients by checking for empty Origin headers:

```go
CheckOrigin: func(r *http.Request) bool {
    origin := r.Header.Get("Origin")
    if origin == "" {
        return true  // Allows non-browser clients
    }
    // ...
}
```

While this is intentional for CLI tools, it opens the door to CSWSH (Cross-Site WebSocket Hijacking) if the authentication middleware is bypassed or misconfigured.

**Recommendation:** Add a configuration option to require Origin validation even for "API" clients, or implement API key authentication for non-browser access:

```go
type WebConfig struct {
    // ...
    RequireOriginValidation bool `yaml:"require_origin_validation"`
}

// In CheckOrigin:
if origin == "" {
    return !cfg.RequireOriginValidation
}
```

---

#### SEC-M2: Rate Limiter State Not Persisted Across Restarts
**File:** `internal/web/auth.go:71-91`  
**Severity:** MEDIUM

The rate limiter tracks failed login attempts in memory only. An attacker could bypass rate limiting by triggering a server restart.

**Recommendation:** Persist rate limiter state to disk or use a distributed rate limiting solution if running multiple instances.

---

#### SEC-M3: Information Disclosure Through Error Messages
**File:** `internal/web/server.go:433-438`  
**Severity:** MEDIUM

```go
result, err := s.store.QueryRangeWithMeta(from, to, points)
if err != nil {
    log.Printf("[API History] query error: %v", err)
    jsonError(w, "internal storage error", http.StatusInternalServerError)
    return
}
```

While the fix in commit `ee57d5b` prevents leaking internal errors to the client, internal error messages may still contain sensitive path information in logs. Consider sanitizing logged errors.

---

#### SEC-M4: Weak Default Argon2 Parameters
**File:** `internal/config/config.go:220-224`  
**Severity:** MEDIUM

Default Argon2 parameters:
```go
Argon2: Argon2Config{
    Time:    1,
    Memory:  64 * 1024,  // 64 MB
    Threads: 4,
}
```

While the example config uses stronger parameters (`time: 3, memory: 32768`), the defaults are weaker than OWASP recommendations (which suggest time ≥ 3).

**Recommendation:** Update defaults to match OWASP recommendations:
```go
Argon2: Argon2Config{
    Time:    3,
    Memory:  65536,  // 64 MB
    Threads: 4,
}
```

---

### 1.3 Low Severity Issues

#### SEC-L1: Missing Security Headers on Static Assets
**File:** `internal/web/server.go:788-844`  
**Severity:** LOW

Static assets don't receive security headers (CSP, X-Frame-Options, etc.) because they're served before the `securityMiddleware` is applied.

**Recommendation:** Apply security middleware to all routes, or add headers explicitly in the static handler.

---

#### SEC-L2: Prometheus Metrics Endpoint Lacks Rate Limiting
**File:** `internal/web/prometheus.go:18-32`  
**Severity:** LOW

The `/metrics` endpoint has no rate limiting, which could be abused for DoS attacks, especially since it performs database queries.

**Recommendation:** Add rate limiting to the metrics endpoint:
```go
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
    ip := getClientIP(r, s.cfg.TrustProxy)
    if !s.auth.Limiter.Allow(ip) {
        http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
        return
    }
    // ... rest of handler
}
```

---

#### SEC-L3: TrustProxy Header Injection Risk
**File:** `internal/web/server.go:753-768`  
**Severity:** LOW

When `TrustProxy` is enabled, the `X-Forwarded-For` header is trusted without validating that the request actually came from a trusted proxy.

**Recommendation:** Document this risk clearly and consider adding a `TrustedProxies` configuration option with IP validation.

---

### 1.4 Security Positives

The codebase demonstrates several excellent security practices:

| Feature | Implementation | Status |
|---------|---------------|--------|
| Password Hashing | Argon2id with configurable parameters | |
| Session Management | Cryptographically random tokens, SHA-256 hashed storage | |
| CSRF Protection | Double-submit cookie pattern with token validation | |
| Rate Limiting | IP-based login attempt limiting (5 attempts/5 min) | |
| Content Security Policy | Nonce-based CSP with strict directives | |
| Secure Cookies | HttpOnly, Secure (when TLS), SameSite=Strict | |
| Input Validation | Strict language validation, size limits on request bodies | |
| Sandboxing | Landlock LSM integration for filesystem/network restrictions | |
| SRI Hashes | Subresource Integrity for JavaScript files | |
| Timing Attack Prevention | `subtle.ConstantTimeCompare` for password/token comparison | |

---

## 2. Code Quality Analysis

### 2.1 High Priority Issues

#### QUAL-H1: Inconsistent Error Handling Patterns
**Files:** Multiple  
**Severity:** HIGH

The codebase has inconsistent error handling - some errors are logged and swallowed, others are returned:

```go
// In auth.go:272 - Error swallowed
if err := json.Unmarshal(data, &saved); err != nil {
    return err  // Good: error returned
}

// In server.go:377 - Error only logged
if err := json.NewEncoder(w).Encode(sample); err != nil {
    log.Printf("JSON encode error: %v", err)  // Bad: error swallowed
}
```

**Recommendation:** Establish a consistent error handling strategy. For HTTP handlers, consider using a helper:

```go
func (s *Server) respondJSON(w http.ResponseWriter, data interface{}) {
    w.Header().Set("Content-Type", "application/json")
    if err := json.NewEncoder(w).Encode(data); err != nil {
        log.Printf("JSON encode error: %v", err)
        // Already started writing, can't change status
    }
}
```

---

#### QUAL-H2: Missing Context Cancellation Checks
**File:** `internal/web/server.go:680-693`  
**Severity:** HIGH

```go
func (h *wsHub) run() {
    for {
        select {
        case client := <-h.regCh:
            // ...
        case client := <-h.unregCh:
            // ...
        }
    }
}
```

The WebSocket hub runs indefinitely without checking for context cancellation, preventing clean shutdown.

**Recommendation:**
```go
func (h *wsHub) run(ctx context.Context) {
    for {
        select {
        case client := <-h.regCh:
            // ...
        case client := <-h.unregCh:
            // ...
        case <-ctx.Done():
            return
        }
    }
}
```

---

### 2.2 Medium Priority Issues

#### QUAL-M1: Magic Numbers Throughout Codebase
**Files:** Multiple  
**Severity:** MEDIUM

Numerous magic numbers without documentation:

```go
// server.go:419
points := 450  // Why 450?

// server.go:425-430
if points > 5000 {  // Why 5000?
    points = 5000
}

// websocket.go:90
conn.SetReadLimit(4096)  // Why 4096?
```

**Recommendation:** Define constants with descriptive names:

```go
const (
    DefaultHistoryPoints     = 450
    MaxHistoryPoints         = 5000
    MinHistoryPoints         = 1
    WebSocketMaxMessageSize  = 4096
    MaxRequestBodySize       = 4096
)
```

---

#### QUAL-M2: Race Condition Potential in Session Cleanup
**File:** `internal/web/auth.go:227-255`  
**Severity:** MEDIUM

```go
func (a *AuthManager) CleanupSessions() {
    a.mu.Lock()
    // ... cleanup sessions ...
    a.mu.Unlock()
    
    // Purge stale rate limiter entries
    a.Limiter.mu.Lock()  // Lock ordering: a.mu -> a.Limiter.mu
    // ...
}
```

While not currently a deadlock risk, the inconsistent lock ordering pattern could lead to issues if the code evolves.

**Recommendation:** Document lock ordering hierarchy or use a single mutex for both structures.

---

#### QUAL-M3: WebSocket Connection Counter Not Atomic
**File:** `internal/web/server.go:46-48, websocket.go:52-68`  
**Severity:** MEDIUM

```go
type Server struct {
    wsMu       sync.Mutex
    wsCount    int
    wsIPCounts map[string]int
}
```

The WebSocket connection counters use a mutex, but operations are spread across multiple functions, making it easy to miss unlock calls.

**Recommendation:** Consider using `atomic.Int32` for the global counter:

```go
type Server struct {
    wsCount    atomic.Int32
    wsIPCounts map[string]int
    wsMu       sync.Mutex  // Only for wsIPCounts
}
```

---

### 2.3 Low Priority Issues

#### QUAL-L1: Unused Import
**File:** `internal/web/prometheus.go:352-353`  
**Severity:** LOW

```go
// Ensure collector types are referenced (avoids import cycle if moved to its own file).
var _ *collector.Sample
```

This is a code smell indicating potential architectural issues. The comment suggests awareness of the problem.

---

#### QUAL-L2: Inefficient String Building in Prometheus Handler
**File:** `internal/web/prometheus.go:47-66`  
**Severity:** LOW

```go
var b strings.Builder
b.Grow(4096)

// Helper closures
gauge := func(name, help, labels string, value float64) {
    fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
    // ...
}
```

Closures capture variables and may cause allocations. For hot paths, consider using methods or inline code.

---

### 2.4 Code Quality Positives

| Aspect | Observation | Rating |
|--------|-------------|--------|
| Code Organization | Clean package structure following Go conventions | Excellent |
| Naming | Clear, descriptive function and variable names | Good |
| Comments | Good documentation, especially for exported functions | Good |
| Test Coverage | Test files present for critical components | Good |
| Error Messages | User-friendly error messages (after recent fixes) | Good |
| Go Version | Uses latest Go 1.26.1 | Excellent |

---

## 3. Performance Analysis

### 3.1 High Priority Issues

#### PERF-H1: Memory Allocation in Hot Path (WebSocket Broadcast)
**File:** `internal/web/server.go:67-73`  
**Severity:** HIGH

```go
func (s *Server) BroadcastSample(sample *collector.Sample) {
    data, err := json.Marshal(sample)  // Allocates every collection interval
    if err != nil {
        return
    }
    s.hub.broadcast(data)
}
```

JSON marshaling happens every collection interval (default 1s), creating GC pressure.

**Recommendation:** Consider using a sync.Pool for byte buffers or a pre-allocated buffer if sample sizes are predictable:

```go
var broadcastPool = sync.Pool{
    New: func() interface{} {
        return make([]byte, 0, 8192)
    },
}

func (s *Server) BroadcastSample(sample *collector.Sample) {
    buf := broadcastPool.Get().([]byte)
    defer broadcastPool.Put(buf[:0])
    
    data, err := json.Marshal(sample)
    if err != nil {
        return
    }
    s.hub.broadcast(data)
}
```

---

#### PERF-H2: Unbounded Goroutine Growth in WebSocket Hub
**File:** `internal/web/server.go:695-711`  
**Severity:** HIGH

```go
func (h *wsHub) broadcast(data []byte) {
    h.mu.RLock()
    defer h.mu.RUnlock()
    
    for client := range h.clients {
        select {
        case client.sendCh <- data:
        default:
            // Client too slow, skip
        }
    }
}
```

Each connected WebSocket client gets a goroutine for the write pump. With default limits of 100 connections, this creates 100+ goroutines just for WebSocket handling.

**Recommendation:** This is acceptable for the scale, but document the resource usage clearly.

---

### 3.2 Medium Priority Issues

#### PERF-M1: Inefficient Time Range Validation
**File:** `internal/web/server.go:409-416`  
**Severity:** MEDIUM

```go
if to.Sub(from) > 31*24*time.Hour {
    jsonError(w, "time range too large, max 31 days allowed", http.StatusBadRequest)
    return
}
if to.Sub(from) < 0 {
    jsonError(w, "time range inverted", http.StatusBadRequest)
    return
}
```

The 31-day calculation happens on every request. Use a constant:

```go
const maxHistoryRange = 31 * 24 * time.Hour
```

---

#### PERF-M2: Sprintf in Hot Path
**File:** `internal/web/server.go:444`  
**Severity:** MEDIUM

```go
log.Printf("[API History] loaded %d samples from tier %d (resolution: %s) for window %s in %v",
    len(result.Samples), result.Tier, result.Resolution, to.Sub(from).Round(time.Second), loadDuration)
```

This log statement formats strings even when the log level would discard the message.

**Recommendation:** Check log level before formatting:

```go
if s.cfg.Logging.Enabled && (s.cfg.Logging.Level == "perf" || s.cfg.Logging.Level == "debug") {
    // Only now do the formatting
    log.Printf("[API History] ...", ...)
}
```

---

### 3.3 Performance Positives

| Feature | Implementation | Benefit |
|---------|---------------|---------|
| Tiered Storage | Ring buffer with multiple resolution tiers | Efficient historical data storage |
| Compression | Optional gzip for HTTP and WebSocket | Reduced bandwidth |
| Connection Limits | Configurable per-IP and global WebSocket limits | DoS protection |
| Buffer Pre-sizing | `strings.Builder.Grow()` in Prometheus handler | Reduced allocations |
| Sliding Window Rate Limiting | Efficient time-based cleanup | Low memory overhead |

---

## 4. Recommendations Summary

### Immediate Actions (High Priority)

1. **SEC-H1:** Implement environment variable support for PostgreSQL passwords
2. **SEC-H2:** Add storage directory permission verification
3. **QUAL-H1:** Standardize error handling across all HTTP handlers
4. **QUAL-H2:** Add context cancellation to WebSocket hub

### Short-term Actions (Medium Priority)

1. **SEC-M1:** Add configuration option for strict WebSocket origin validation
2. **SEC-M2:** Persist rate limiter state across restarts
3. **SEC-M4:** Update default Argon2 parameters to OWASP recommendations
4. **QUAL-M1:** Replace magic numbers with named constants
5. **PERF-H1:** Implement buffer pooling for WebSocket broadcasts

### Long-term Actions (Low Priority)

1. **SEC-L1:** Apply security headers to static assets
2. **SEC-L2:** Add rate limiting to Prometheus metrics endpoint
3. **QUAL-L2:** Optimize Prometheus handler to reduce allocations
4. Add comprehensive integration tests for authentication flows
5. Implement structured logging (JSON) for production deployments

---

## 5. Code Snippets for Key Fixes

### Fix 1: Secure PostgreSQL Password Handling

```go
// internal/config/config.go
type PostgresConfig struct {
    Enabled  bool   `yaml:"enabled"`
    Host     string `yaml:"host"`
    Port     int    `yaml:"port"`
    User     string `yaml:"user"`
    Password string `yaml:"password"`
    // New field for environment variable override
    PasswordFromEnv string `yaml:"password_from_env"`
    DBName   string `yaml:"dbname"`
    SSLMode  string `yaml:"sslmode"`
}

func (p *PostgresConfig) GetPassword() string {
    if p.PasswordFromEnv != "" {
        if envPass := os.Getenv(p.PasswordFromEnv); envPass != "" {
            return envPass
        }
    }
    return p.Password
}
```

### Fix 2: Context-Aware WebSocket Hub

```go
// internal/web/server.go
func (s *Server) Start() error {
    // ...
    ctx, cancel := context.WithCancel(context.Background())
    s.hubCancel = cancel
    go s.hub.run(ctx)
    // ...
}

func (s *Server) Shutdown(ctx context.Context) error {
    if s.hubCancel != nil {
        s.hubCancel()
    }
    // ... rest of shutdown
}

// internal/web/websocket.go (wsHub)
func (h *wsHub) run(ctx context.Context) {
    for {
        select {
        case client := <-h.regCh:
            h.mu.Lock()
            h.clients[client] = true
            h.mu.Unlock()
        case client := <-h.unregCh:
            h.mu.Lock()
            delete(h.clients, client)
            h.mu.Unlock()
        case <-ctx.Done():
            // Clean shutdown: close all client connections
            h.mu.Lock()
            for client := range h.clients {
                close(client.sendCh)
            }
            h.mu.Unlock()
            return
        }
    }
}
```

### Fix 3: Constants for Magic Numbers

```go
// internal/web/server.go
const (
    DefaultHistoryPoints    = 450
    MaxHistoryPoints        = 5000
    MinHistoryPoints        = 1
    MaxHistoryRange         = 31 * 24 * time.Hour
    MaxRequestBodySize      = 4096
    WebSocketReadLimit      = 4096
    SessionCleanupInterval  = 5 * time.Minute
)
```

---

## 6. Conclusion

Kula is a well-designed monitoring tool with strong security fundamentals. The recent commit `ee57d5b` demonstrates active attention to security issues (fixing internal error detail leakage). The codebase follows Go best practices and has good architectural separation of concerns.

**Key Strengths:**
- Modern security features (Landlock, Argon2, CSRF protection)
- Clean, maintainable code structure
- Good documentation and configuration examples
- Active maintenance and security awareness

**Areas for Improvement:**
- Credential handling in configuration
- Consistent error handling patterns
- Context propagation for graceful shutdown
- Performance optimization for high-frequency operations

The project is suitable for production use with the recommended security hardening applied.

---

## Appendix A: Severity Legend

| Severity | Description |
|----------|-------------|
| **CRITICAL** | Immediate security risk, could lead to system compromise |
| **HIGH** | Significant security risk or major functionality issue |
| **MEDIUM** | Moderate risk, should be addressed in next release |
| **LOW** | Minor issue, cosmetic or low-impact |

## Appendix B: Scoring Methodology

| Score | Description |
|-------|-------------|
| 90-100 | Excellent - Industry-leading practices |
| 80-89 | Good - Above average with minor issues |
| 70-79 | Acceptable - Meets standards with room for improvement |
| 60-69 | Below Average - Significant issues need attention |
| <60 | Poor - Major rework required |

---

*Report generated by automated code analysis supplemented with manual review.*
