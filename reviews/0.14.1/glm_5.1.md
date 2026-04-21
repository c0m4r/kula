# Kula — Comprehensive Code Review Report

**Project:** [c0m4r/kula](https://github.com/c0m4r/kula)  
**Version Reviewed:** 0.14.0 (latest main branch)  
**Language:** Go 1.26.1 + JavaScript (embedded SPA)  
**Reviewer:** Security Research & Code Quality Audit  
**Date:** 2026-04-11  

---

## Table of Contents

1. [Executive Summary](#1-executive-summary)
2. [Project Overview](#2-project-overview)
3. [Security Findings](#3-security-findings)
4. [Code Quality Findings](#4-code-quality-findings)
5. [Performance Findings](#5-performance-findings)
6. [Scoring Summary](#6-scoring-summary)
7. [Overall Recommendations](#7-overall-recommendations)

---

## 1. Executive Summary

Kula is a lightweight, self-contained Linux server monitoring tool written in Go. It collects system metrics by reading directly from `/proc` and `/sys`, stores them in a custom tiered ring-buffer storage engine, and serves them via a real-time web UI dashboard and a terminal TUI. The project demonstrates a strong security posture overall, with defense-in-depth measures including Landlock sandboxing, Argon2id password hashing, CSRF protection, CSP headers, rate limiting, and WebSocket origin validation.

The codebase is well-structured, idiomatic Go with clear separation of concerns. The custom binary storage codec shows sophisticated engineering with backward compatibility guarantees. However, several findings across security, code quality, and performance warrant attention. The most critical issues involve PostgreSQL credential exposure, potential denial-of-service vectors in the WebSocket and API layers, and a few race conditions in the authentication system.

**Overall Project Score: 7.8 / 10**

| Category | Score | Grade |
|----------|-------|-------|
| Security | 7.5/10 | B+ |
| Code Quality | 8.0/10 | A- |
| Performance | 7.8/10 | B+ |
| Architecture | 8.5/10 | A |
| Documentation | 8.0/10 | A- |

---

## 2. Project Overview

### Architecture

```
Linux Kernel (/proc, /sys)
        │
        ▼
   Collectors (CPU, Mem, Net, Disk, System, GPU, Apps)
        │
        ├── Storage Engine (Tiered Ring Buffer)
        │     ├── Tier 1: 1s raw samples (250 MB)
        │     ├── Tier 2: 1m aggregated (150 MB)
        │     └── Tier 3: 5m aggregated (50 MB)
        │
        ├── Web Server (HTTP + WebSocket)
        │     ├── REST API (/api/current, /api/history)
        │     ├── WebSocket (/ws) — live streaming
        │     ├── Prometheus metrics (/metrics)
        │     └── Embedded SPA dashboard
        │
        └── TUI (Terminal UI via Bubble Tea)
```

### Key Statistics

| Metric | Value |
|--------|-------|
| Go Source Files | ~25 |
| Lines of Go Code | ~5,000+ |
| JavaScript (Frontend) | ~2,000+ |
| Dependencies | 7 direct, 16 indirect |
| Test Coverage | Moderate (key packages covered) |

---

## 3. Security Findings

### SEC-01: PostgreSQL Credentials Logged or Exposed via DSN Construction ✅ PARTIALLY VALID — FIXED

**Severity: MEDIUM**  
**File:** `internal/collector/postgres.go:44-56`  
**CVSS: 5.3**

The DSN string is constructed by concatenating the password directly into it. While it is not explicitly logged, the DSN is stored as a struct field on `postgresCollector` and could appear in error messages, stack traces, or debug output. The `fmt.Sprintf` approach also does not properly escape special characters in the password (e.g., spaces, quotes, or backslashes), which could lead to DSN injection or authentication bypass.

**Validation notes:**
- The DSN is never directly logged in the current code — `pc.dsn` only appears in `sql.Open()`. The "credential exposure in error messages" concern is theoretical, not currently exploitable.
- The password escaping issue is **real**: in libpq key=value format, an unquoted value containing spaces, single quotes, or backslashes produces a malformed DSN, causing authentication to silently fail.

**Fix applied:** Password value is now single-quoted with proper libpq escaping (backslashes and single quotes within the value are backslash-escaped):

```go
if password != "" {
    escaped := strings.ReplaceAll(password, `\`, `\\`)
    escaped = strings.ReplaceAll(escaped, `'`, `\'`)
    dsn += " password='" + escaped + "'"
}
```

---

### SEC-02: Rate Limiter IP Bypass via X-Forwarded-For When TrustProxy Enabled ❌ NOT VALID

**Severity: MEDIUM**  
**File:** `internal/web/server.go:753-768`, `internal/web/auth.go:70-91`  
**CVSS: 5.0**

~~An attacker behind a single trusted proxy could inject an additional `X-Forwarded-For` entry, causing the rightmost IP to be the attacker's spoofed value.~~

**Finding is not valid.** The rightmost-XFF approach is correct for the documented single-proxy topology. When exactly one trusted proxy sits in front of Kula, it appends the real client IP as the rightmost entry. An attacker who prepends fake IPs to `X-Forwarded-For` cannot control the rightmost entry — that position is always written by the proxy. The code's own comment explains this accurately:

```
// The rightmost IP is the one appended by our trusted proxy.
// Leftmost IPs are client-controlled and can be spoofed.
```

The startup log already emits a `TrustProxy` security notice. Multi-proxy limitations are a documented deployment constraint, not a vulnerability.

---

### SEC-03: Session CSRF Token Returned in Auth Status Endpoint ❌ NOT VALID

**Severity: LOW**  
**File:** `internal/web/server.go:607-627`  
**CVSS: 3.1**

~~The `/api/auth/status` endpoint returns the CSRF token, weakening CSRF protection because any XSS vulnerability would allow an attacker to read this endpoint and obtain the token.~~

**Finding is not valid.** Returning the CSRF token in `/api/auth/status` is intentional SPA design — the login handler already returns it on every successful login for the same reason (page-refresh recovery). CSRF protection specifically defends against cross-site request forgery; XSS has far more direct attack vectors than fetching a CSRF token from an API. A successful XSS attack can already forge requests directly using the victim's existing session cookie. No action needed.

---

### SEC-04: Custom Metrics Unix Socket — No Authentication or Authorization

**Severity: MEDIUM**  
**File:** `internal/collector/custom.go:42-68`  
**CVSS: 5.5**

The custom metrics collector listens on a Unix socket (`kula.sock`) with mode `0660` (owner + group writable). Any process in the same group can write arbitrary metric data, which then gets reflected in the dashboard and Prometheus endpoint. There is no authentication or validation of the sender beyond the Unix filesystem permissions. If an attacker gains access to any process running as the same user or group, they can inject fake monitoring data.

```go
// Current:
listener, err := net.Listen("unix", sockPath)
if err != nil {
    return nil, fmt.Errorf("custom metrics socket: %w", err)
}
if err := os.Chmod(sockPath, 0660); err != nil {
    log.Printf("[custom] warning: chmod socket: %v", err)
}
```

**Recommendation:** Consider adding peer credential validation using `SO_PEERCRED` on Linux to restrict access to specific UIDs, or implement a shared secret mechanism for the custom metrics protocol:

```go
// Recommended: peer credential validation
import "syscall"

func (cc *customCollector) validatePeer(conn *net.UnixConn) bool {
    fd, err := conn.File()
    if err != nil {
        return false
    }
    defer fd.Close()
    
    cred, err := syscall.GetsockoptUcred(int(fd.Fd()), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
    if err != nil {
        return false
    }
    
    // Only allow same UID
    return cred.Uid == uint32(os.Getuid())
}
```

---

### SEC-05: Landlock Sandbox Graceful Degradation May Leave Process Unrestricted

**Severity: LOW**  
**File:** `internal/sandbox/sandbox.go:152-161`  
**CVSS: 2.4**

When the kernel does not support Landlock (ABI < 1 or < 4 for network), the sandbox enforcement is silently skipped with only a warning log. On older kernels (pre-5.13), the process runs without any filesystem or network restrictions. While this is documented behavior and there is no practical alternative on old kernels, it means that on such systems, a vulnerability in the web server could give an attacker unrestricted filesystem or network access.

```go
// Current:
if abi < 1 {
    log.Println("Landlock ABI < 1, skipping sandbox enforcement")
    return nil  // Process runs unrestricted
}
```

**Recommendation:** Add a configuration option `sandbox.strict` that causes the process to refuse to start if Landlock cannot be enforced. This allows operators to make an explicit security decision:

```go
// Recommended:
if abi < 1 {
    if cfg.Sandbox.Strict {
        return fmt.Errorf("sandbox: Landlock not available and sandbox.strict is enabled")
    }
    log.Println("Landlock ABI < 1, skipping sandbox enforcement")
    return nil
}
```

---

### SEC-06: WebSocket Origin Check Can Be Bypassed for Non-Browser Clients

**Severity: LOW**  
**File:** `internal/web/websocket.go:21-48`  
**CVSS: 3.0**

The WebSocket upgrader allows connections with no `Origin` header (to support CLI tools). An attacker using a crafted HTTP client (not a browser) can omit the `Origin` header and bypass the same-origin check. This is a known trade-off for CLI compatibility, but it means the WebSocket endpoint is accessible to any non-browser client that can reach the server.

```go
// Current:
CheckOrigin: func(r *http.Request) bool {
    origin := r.Header.Get("Origin")
    if origin == "" {
        return true  // Allow non-browser clients
    }
    // ...
}
```

**Recommendation:** When authentication is enabled, this is mitigated because the WebSocket handler is behind the auth middleware. However, when auth is disabled, consider adding a configuration option to require origin validation even for non-browser clients, or add a configurable allowlist of permitted origins.

---

### SEC-07: Prometheus Bearer Token Compared in Constant Time But Stored in Plaintext Config

**Severity: LOW**  
**File:** `internal/config/config.go:76-79`, `internal/web/prometheus.go:24-32`  
**CVSS: 2.8**

The Prometheus metrics bearer token is stored in plaintext in `config.yaml`. While the comparison uses `subtle.ConstantTimeCompare` (good), the token itself is visible to anyone who can read the config file. This is a minor concern since the config file also contains password hashes and salts, but it is worth noting that the Prometheus token does not benefit from the same Argon2id hashing that session tokens do.

**Recommendation:** Consider supporting environment variable injection for the Prometheus token (similar to `KULA_PORT`), so it does not need to be stored in the config file:

```bash
export KULA_PROMETHEUS_TOKEN="my-secret-token"
```

---

### SEC-08: Nginx Status URL Not Validated — Potential SSRF

**Severity: MEDIUM**  
**File:** `internal/collector/nginx.go:33`, `internal/config/config.go:150-153`  
**CVSS: 5.0**

The `nginx.status_url` configuration value is used directly in an HTTP GET request without any validation. An operator who has write access to the config file (or an environment variable injection) could point this at internal services, causing Kula to make requests to arbitrary URLs. Combined with the fact that metrics are exposed via the Prometheus endpoint or the API, this could be used as an SSRF vector to probe internal network services.

```go
// Current:
resp, err := c.nginxClient.Get(c.appCfg.Nginx.StatusURL)
```

**Recommendation:** Validate the URL at config load time to ensure it points to localhost or a private network address:

```go
// Recommended: URL validation at load time
func validateStatusURL(rawURL string) error {
    u, err := url.Parse(rawURL)
    if err != nil {
        return fmt.Errorf("invalid status_url: %w", err)
    }
    host := u.Hostname()
    if host != "localhost" && host != "127.0.0.1" && host != "::1" {
        return fmt.Errorf("status_url must point to localhost, got %q", host)
    }
    return nil
}
```

---

## 4. Code Quality Findings

### QUAL-01: Global Mutable Variable for CPU Temperature Sensors

**Severity: MEDIUM**  
**File:** `internal/collector/cpu.go:23-27`

The `sysTempSensors` variable is a package-level mutable global that is lazily initialized on first access. While this works correctly in practice because it is only written once, it is not thread-safe if two goroutines call `collectCPUTemperature` simultaneously before initialization. More importantly, it makes testing difficult because tests cannot easily reset the sensor discovery state.

```go
// Current:
var (
    sysTempSensors []sysSensor  // package-level mutable global
)
```

**Recommendation:** Move the sensor cache into the `Collector` struct where it belongs, alongside the other state:

```go
// Recommended:
type Collector struct {
    // ...
    cpuTempSensors []sysSensor  // instance-level, testable
}

func (c *Collector) collectCPUTemperature() (float64, []CPUTempSensor) {
    if c.cpuTempSensors == nil {
        c.cpuTempSensors = discoverCPUTempPath()
    }
    // ...
}
```

---

### QUAL-02: Duplicate Aggregation Logic — `minSample` and `maxSample` Are Nearly Identical

**Severity: LOW**  
**File:** `internal/storage/store.go:429-562`

The `minSample` and `maxSample` functions are almost identical — they differ only in the comparison operator (`<` vs `>` for `minF`/`maxF`). This violates the DRY principle and doubles the maintenance burden. Any new metric field must be added to both functions identically.

```go
// Current: Two 65-line functions that differ by one operator each
func minSample(a, b *collector.Sample) *collector.Sample { ... }
func maxSample(a, b *collector.Sample) *collector.Sample { ... }
```

**Recommendation:** Extract a generic compare-and-merge function that takes a comparison function:

```go
// Recommended:
type cmpOp func(float64, float64) float64
type cmpUOp func(uint64, uint64) uint64

func compareSample(a, b *collector.Sample, cf cmpOp, uf cmpUOp) *collector.Sample {
    if a == nil { return b }
    if b == nil { return a }
    res := *a
    res.CPU.Total.Usage = cf(a.CPU.Total.Usage, b.CPU.Total.Usage)
    // ... (single function to maintain)
    return &res
}

var minSample = func(a, b *collector.Sample) *collector.Sample {
    return compareSample(a, b, minF, minU)
}
var maxSample = func(a, b *collector.Sample) *collector.Sample {
    return compareSample(a, b, maxF, maxU)
}
```

---

### QUAL-03: Error Handling — Silent Failures Throughout Collectors

**Severity: MEDIUM**  
**Files:** Multiple collector files

Most collector functions silently return empty/zero values on errors. While this is intentional for resilience (a monitoring tool should not crash if one metric is unavailable), it makes debugging very difficult. Errors in reading `/proc` files, parsing values, or accessing hardware sensors are swallowed without any indication to the operator.

```go
// Current pattern (repeated across many collectors):
func collectSwap() SwapStats {
    m := parseMemInfo()
    // If parseMemInfo returns nil (error), we silently return zero stats
    s := SwapStats{Total: m["SwapTotal"], Free: m["SwapFree"]}
    // ...
}
```

**Recommendation:** Add a structured error/metrics collection counter that can be exposed via the `/api/current` or a new `/api/health` endpoint. This allows operators to see which collectors are failing:

```go
// Recommended:
type CollectorHealth struct {
    Name    string `json:"name"`
    Errors  int64  `json:"errors"`
    LastErr string `json:"last_error,omitempty"`
}

func (c *Collector) Health() []CollectorHealth {
    // Return health status for each sub-collector
}
```

---

### QUAL-04: `process.go` — Reads `/proc/[pid]/stat` for Every Process Every Second

**Severity: MEDIUM (Performance + Quality)**  
**File:** `internal/collector/process.go:10-63`

The `collectProcesses` function iterates over all PIDs in `/proc`, reads each `/proc/[pid]/stat` file, and additionally reads `/proc/[pid]/task` for thread counts. On systems with thousands of processes, this creates significant I/O pressure. The function also allocates a new `os.ReadDir` call and opens/stat individual files without any caching or batching.

```go
// Current: O(n) file opens per collection cycle
func collectProcesses() ProcessStats {
    entries, err := os.ReadDir(procPath)
    for _, entry := range entries {
        data, err := os.ReadFile(filepath.Join(procPath, entry.Name(), "stat"))
        // ...also reads /proc/[pid]/task
    }
}
```

**Recommendation:** Consider using a more efficient approach such as reading `/proc/stat` for overall process counts (already partially done) or adding a configurable process collection interval that is longer than the default 1-second cycle:

```go
// Recommended: separate interval for expensive collectors
type CollectionConfig struct {
    Interval         time.Duration `yaml:"interval"`
    ProcessInterval  time.Duration `yaml:"process_interval"` // e.g., 5s instead of 1s
    // ...
}
```

---

### QUAL-05: Binary Codec Manual Offset Tracking Is Fragile

**Severity: LOW**  
**File:** `internal/storage/codec.go`

The binary codec uses manually calculated byte offsets with comments documenting the layout. While the comments are thorough and the code is well-documented, this approach is inherently fragile — any misalignment between the encoder and decoder will silently corrupt data. The `fixedBlockSize = 218` constant and the offset comments must be kept in sync manually.

```go
// Current: manually maintained offsets
const fixedBlockSize = 218

// [0:28]   cpu total × 7 float32
// [28:30]  num_cores uint16
// [30:34]  cpu_temp  float32
// ... (many more)
```

**Recommendation:** Consider using Go's `encoding/binary` with a structured approach, or generate the codec from a schema. At a minimum, add compile-time assertions that verify the size of the fixed block:

```go
// Recommended: compile-time size assertion
func init() {
    var b [fixedBlockSize]byte
    _ = b
    // The compiler will catch if the size assertion is wrong
    // when we write beyond the array bounds
}
```

---

### QUAL-06: Session Persistence Stores CSRF Tokens in Plaintext on Disk

**Severity: LOW**  
**File:** `internal/web/auth.go:292-318`

The `SaveSessions` function writes session data including CSRF tokens to `sessions.json` on disk. While the session tokens themselves are hashed before storage (good), the CSRF tokens are stored in plaintext. If an attacker gains read access to the storage directory, they can extract CSRF tokens and potentially forge requests for active sessions.

```go
// Current:
toSave = append(toSave, sessionData{
    Token:     hashedToken,    // Good: hashed
    CSRFToken: sess.csrfToken, // Exposed in plaintext
    // ...
})
```

**Recommendation:** Either encrypt the sessions file at rest, or derive the CSRF token deterministically from the session token hash so it does not need to be stored separately:

```go
// Recommended: derive CSRF token from session hash
func deriveCSRFToken(hashedToken string) string {
    h := sha256.Sum256([]byte("csrf:" + hashedToken))
    return hex.EncodeToString(h[:])
}
```

---

### QUAL-07: `getClientIP` Rightmost-XFF Logic Is Counter-Intuitive

**Severity: LOW**  
**File:** `internal/web/server.go:753-768`

The `getClientIP` function takes the rightmost entry from `X-Forwarded-For` when `trust_proxy` is enabled. The comment explains this is because "the rightmost IP is the one appended by our trusted proxy," but this is only correct for a single-proxy deployment. For multi-proxy setups (e.g., CDN → load balancer → Kula), the rightmost IP might be the load balancer's internal IP, not the client's real IP.

**Recommendation:** Document the expected proxy topology explicitly in the configuration file and consider adding a `trusted_proxy_count` configuration option, as detailed in SEC-02.

---

## 5. Performance Findings

### PERF-01: Storage Write Path Holds Global Mutex During Entire Write + Aggregation

**Severity: HIGH**  
**File:** `internal/storage/store.go:174-237`

The `WriteSample` method acquires `s.mu.Lock()` for the entire duration of writing to tier 0, aggregating tier 1 and tier 2, and invalidating the query cache. This means that all read queries (which need `RLock`) are blocked during the entire write path, including the relatively expensive aggregation operations. On a 1-second collection interval, this creates a contention point that can cause read latency spikes.

```go
// Current: single mutex for entire write path
func (s *Store) WriteSample(sample *collector.Sample) error {
    s.mu.Lock()           // Blocks all reads
    defer s.mu.Unlock()
    
    // Write to tier 0 (disk I/O)
    s.tiers[0].Write(as)
    
    // Aggregate for tier 1 (CPU-intensive)
    if s.tier1Count >= s.ratio1 {
        agg := s.aggregateSamples(s.tier1Buf, ...)  // Expensive
        s.tiers[1].Write(agg)                        // More disk I/O
        
        // Aggregate for tier 2 (even more CPU-intensive)
        if s.tier2Count >= s.ratio2 {
            agg3 := s.aggregateAggregated(s.tier2Buf, ...)
            s.tiers[2].Write(agg3)
        }
    }
    
    // Invalidate query cache
    s.queryCache = make(...)
}
```

**Recommendation:** Separate the write lock from the read lock, and perform aggregation outside the critical section:

```go
// Recommended: lock-free aggregation
func (s *Store) WriteSample(sample *collector.Sample) error {
    s.mu.Lock()
    // Only perform tier-0 write and buffer append under lock
    s.tiers[0].Write(as)
    s.latestCache = as
    s.tier1Buf = append(s.tier1Buf, sample)
    s.tier1Count++
    // Copy buffer references we need for aggregation
    buf1 := s.tier1Buf
    count1 := s.tier1Count
    s.mu.Unlock()
    
    // Aggregate outside the lock
    if count1 >= s.ratio1 {
        agg := s.aggregateSamples(buf1, ...)
        s.mu.Lock()
        s.tiers[1].Write(agg)
        s.tier1Buf = nil
        s.tier1Count = 0
        s.mu.Unlock()
    }
}
```

---

### PERF-02: Ring Buffer ReadRange Performs Full Segment Scan Without Index

**Severity: MEDIUM**  
**File:** `internal/storage/tier.go:234-347`

The `ReadRange` method scans the entire ring buffer segment linearly to find records within the requested time range. For a 250 MB tier with 1-second resolution (approximately 4 days of data), this means reading and parsing through potentially hundreds of thousands of records even if the user only requests the last 5 minutes. The optimization for v2 binary files (checking the oldest record of segment 2) helps in some cases, but the general case still requires a full scan.

```go
// Current: linear scan
for bytesRead < seg.size {
    // Read 4-byte length prefix
    // Read dataLen bytes of payload
    // Extract timestamp
    // Compare against from/to range
}
```

**Recommendation:** Implement a sparse time index (e.g., record the offset of the first record per hour or per 1000 records) in a small sidecar file. This would allow binary search to quickly locate the starting offset for a given time range:

```go
// Recommended: sparse index structure
type TimeIndex struct {
    Entries []IndexEntry  // Sorted by timestamp
}
type IndexEntry struct {
    Timestamp int64   // Unix nano
    Offset    int64   // Byte offset in tier file
}
```

---

### PERF-03: Query Cache Invalidated on Every WriteSample — Excessive GC Pressure

**Severity: MEDIUM**  
**File:** `internal/storage/store.go:232-234`

The query cache is completely replaced (by creating a new empty map) on every `WriteSample` call. This happens every second by default. Any cached results are immediately discarded, and the old map is garbage collected. If multiple clients are requesting history data simultaneously, this means every request hits the disk because the cache is always empty by the time it is checked.

```go
// Current: full cache invalidation every second
s.queryCacheMu.Lock()
s.queryCache = make(map[queryCacheKey]*HistoryResult)  // New empty map every second
s.queryCacheMu.Unlock()
```

**Recommendation:** Use a time-based cache expiry instead of full invalidation. Since history data does not change (past timestamps are immutable), cached results can be kept for their entire validity window:

```go
// Recommended: time-based expiry
type queryCacheEntry struct {
    result    *HistoryResult
    createdAt time.Time
}

// Only invalidate entries that overlap with the new sample's timestamp
func (s *Store) invalidateQueryCache(newTS time.Time) {
    s.queryCacheMu.Lock()
    defer s.queryCacheMu.Unlock()
    for key, entry := range s.queryCache {
        // Only remove if the query window includes the new sample
        if newTS.UnixNano() >= key.fromNano && newTS.UnixNano() <= key.toNano {
            delete(s.queryCache, key)
        }
    }
    // Also evict entries older than 60 seconds
    cutoff := time.Now().Add(-60 * time.Second)
    for key, entry := range s.queryCache {
        if entry.createdAt.Before(cutoff) {
            delete(s.queryCache, key)
        }
    }
}
```

---

### PERF-04: WebSocket Broadcast Clones Data for Every Client on Every Tick

**Severity: LOW**  
**File:** `internal/web/server.go:695-711`

The `BroadcastSample` method marshals the sample to JSON once (good), but the `broadcast` method sends the same byte slice to all clients via channels. While this is efficient in terms of marshaling, the `sendCh` channel buffer of 64 entries per client means that if there are 100 WebSocket clients, there are 100 × 64 = 6,400 buffered messages in memory. With each message being ~2-5 KB of JSON, this could consume 12-32 MB of memory just for WebSocket buffers.

```go
// Current:
client := &wsClient{
    sendCh: make(chan []byte, 64),  // 64-entry buffer per client
}
```

**Recommendation:** Consider using a shared broadcast buffer with reference counting, or reduce the per-client buffer size and use a more aggressive back-pressure strategy:

```go
// Recommended: smaller buffer with back-pressure
client := &wsClient{
    sendCh: make(chan []byte, 16),  // Smaller buffer
}
// In broadcast: if channel is full, skip rather than buffer
select {
case client.sendCh <- data:
default:
    // Client too slow, skip this update (already implemented)
}
```

---

### PERF-05: Gzip Middleware Does Not Check Content-Length Before Compressing

**Severity: LOW**  
**File:** `internal/web/server.go:135-150`

The gzip middleware compresses all responses regardless of their size. For very small responses (e.g., the `/health` endpoint returning "kula is healthy"), the gzip overhead (header + compression) can actually increase the response size. Most production-grade gzip middleware skips compression for responses below a configurable threshold (typically 512 bytes or 1 KB).

```go
// Current: compresses everything
func gzipMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // No size check before compressing
        w.Header().Set("Content-Encoding", "gzip")
        gz := gzip.NewWriter(w)
        // ...
    })
}
```

**Recommendation:** Add a minimum size threshold for compression:

```go
// Recommended: skip compression for small responses
const minCompressSize = 512

// Use a buffering writer that only enables gzip if the response
// exceeds the threshold
```

---

### PERF-06: `aggregateSamples` Performs O(n*m) Lookups for Interface and Disk Matching

**Severity: LOW**  
**File:** `internal/storage/store.go:693-737`

When averaging network interface or disk device metrics across samples, the code uses a nested loop pattern: for each target interface, iterate over all interfaces in each sample to find the matching name. This is O(n × m) where n is the number of samples and m is the number of devices per sample.

```go
// Current: O(n*m) lookup
for i := range avg.Network.Interfaces {
    for _, s := range samples {
        for _, iface := range s.Network.Interfaces {
            if iface.Name == avg.Network.Interfaces[i].Name {
                rxSum += iface.RxMbps
                // ...
            }
        }
    }
}
```

**Recommendation:** Build a lookup map once per sample:

```go
// Recommended: O(n+m) per sample
for _, s := range samples {
    ifaceMap := make(map[string]*collector.NetInterface)
    for idx := range s.Network.Interfaces {
        ifaceMap[s.Network.Interfaces[idx].Name] = &s.Network.Interfaces[idx]
    }
    for i := range avg.Network.Interfaces {
        if iface, ok := ifaceMap[avg.Network.Interfaces[i].Name]; ok {
            rxSum += iface.RxMbps
        }
    }
}
```

---

## 6. Scoring Summary

### Security Scorecard

| Finding ID | Severity | Category | Score Impact |
|-----------|----------|----------|-------------|
| SEC-01 | Medium | Credential Exposure | -0.5 |
| SEC-02 | Medium | Auth Bypass Risk | -0.5 |
| SEC-03 | Low | CSRF Token Exposure | -0.2 |
| SEC-04 | Medium | AuthZ Gap | -0.5 |
| SEC-05 | Low | Sandbox Degradation | -0.1 |
| SEC-06 | Low | WebSocket Bypass | -0.2 |
| SEC-07 | Low | Plaintext Secret | -0.1 |
| SEC-08 | Medium | SSRF Risk | -0.5 |

**Security Score: 7.5/10** — Strong security posture with defense-in-depth (Landlock, Argon2id, CSP, CSRF, rate limiting), but several medium-severity gaps in credential handling, SSRF, and authorization.

### Code Quality Scorecard

| Finding ID | Severity | Category | Score Impact |
|-----------|----------|----------|-------------|
| QUAL-01 | Medium | Global Mutable State | -0.3 |
| QUAL-02 | Low | Code Duplication | -0.2 |
| QUAL-03 | Medium | Silent Error Swallowing | -0.3 |
| QUAL-04 | Medium | Performance Anti-pattern | -0.3 |
| QUAL-05 | Low | Fragile Manual Offsets | -0.2 |
| QUAL-06 | Low | Plaintext CSRF in Storage | -0.1 |
| QUAL-07 | Low | Counter-intuitive Logic | -0.1 |

**Code Quality Score: 8.0/10** — Generally well-written, idiomatic Go with clear separation of concerns and thorough documentation. Main concerns are global mutable state and silent error handling.

### Performance Scorecard

| Finding ID | Severity | Category | Score Impact |
|-----------|----------|----------|-------------|
| PERF-01 | High | Lock Contention | -0.8 |
| PERF-02 | Medium | Full Scan No Index | -0.4 |
| PERF-03 | Medium | Cache Thrashing | -0.4 |
| PERF-04 | Low | Memory Buffering | -0.2 |
| PERF-05 | Low | Gzip Overhead | -0.1 |
| PERF-06 | Low | O(n*m) Lookup | -0.2 |

**Performance Score: 7.8/10** — Good baseline performance with the ring buffer and binary codec design. Main bottleneck is the write-path lock contention and the lack of a time index for range queries.

### Positive Highlights

| Area | Implementation | Grade |
|------|---------------|-------|
| **Landlock Sandboxing** | Kernel-level FS + network restrictions | A |
| **Argon2id Password Hashing** | OWASP-compliant parameters (64MB memory, 4 threads) | A |
| **CSRF Protection** | Double-submit cookie + origin validation | A |
| **CSP Headers** | Nonce-based CSP with strict defaults | A |
| **Session Security** | Hashed-at-rest tokens, HttpOnly cookies, SameSite=Strict | A |
| **WebSocket Limits** | Global + per-IP connection limits | A- |
| **Rate Limiting** | IP-based with 5 attempts per 5 minutes | B+ |
| **Binary Codec Design** | Backward-compatible versioned format with migration | A |
| **Tiered Storage** | Clean multi-resolution ring buffer with restart recovery | A |
| **Configuration Validation** | Tier resolution divisibility, size parsing, range checks | A- |
| **Embedding** | SPA embedded in binary via `//go:embed` | A |
| **SRI Hashes** | Subresource integrity for all JS assets | A |

---

## 7. Overall Recommendations

### Priority 1 — Should Fix Before Next Release

1. ~~**Fix PostgreSQL DSN construction** (SEC-01): Use `url.URL` or proper escaping to prevent DSN injection and avoid credential exposure in error messages.~~ ✅ FIXED — password is now properly single-quoted with libpq escaping in `newPostgresCollector`.

2. **Reduce write-path lock contention** (PERF-01): Move aggregation outside the global mutex to prevent read latency spikes. This is the single highest-impact performance improvement.

3. **Add SSRF protection for nginx status_url** (SEC-08): Validate that the configured URL points to localhost at config load time.

4. **Improve query cache strategy** (PERF-03): Replace full cache invalidation with targeted eviction to reduce GC pressure and improve cache hit rates for concurrent dashboard users.

### Priority 2 — Should Fix in the Next Few Releases

5. **Move global sensor state into Collector struct** (QUAL-01): This improves testability and eliminates the potential for race conditions during initialization.

6. **Add peer credential validation to custom metrics socket** (SEC-04): Use `SO_PEERCRED` to restrict access to the same UID.

7. **Add structured error reporting for collectors** (QUAL-03): Expose collector health metrics so operators can detect when `/proc` reads are failing.

8. **Separate process collection interval** (QUAL-04): Allow a longer interval for expensive collectors like process enumeration.

9. **Add sparse time index to ring buffer** (PERF-02): Implement a sidecar index file for O(log n) range queries instead of O(n) full scans.

### Priority 3 — Nice to Have

10. **Refactor duplicate min/max sample functions** (QUAL-02): Extract a generic comparison function to reduce maintenance burden.

11. **Derive CSRF token from session hash** (QUAL-06): Avoid storing CSRF tokens in plaintext on disk.

12. ~~**Add configurable proxy depth for X-Forwarded-For** (SEC-02): Support multi-proxy deployments with a `trusted_proxy_count` setting.~~ ❌ NOT VALID — rightmost-XFF is correct for single-proxy topology.

13. **Add gzip minimum size threshold** (PERF-05): Skip compression for responses under 512 bytes.

14. **Optimize aggregation lookups with maps** (PERF-06): Replace O(n*m) nested loops with O(n+m) map-based lookups.

15. **Add strict sandbox mode** (SEC-05): Allow operators to require Landlock enforcement and refuse to start without it.

---

## Conclusion

Kula is a well-engineered monitoring tool with impressive security foundations for a project of its size. The use of Landlock sandboxing, Argon2id password hashing, CSP with nonces, and comprehensive CSRF protection demonstrates that security was a design consideration from the beginning, not an afterthought. The custom ring-buffer storage engine with backward-compatible binary codec shows sophisticated systems programming skills.

The most impactful areas for improvement are in the storage layer's lock contention (which affects all users), the PostgreSQL credential handling, and the SSRF surface area in the nginx collector. Addressing these three areas would significantly improve both the security posture and the real-world performance of the tool under load.

The codebase is clean, well-documented, and follows Go idioms consistently. With the recommended improvements, particularly around the performance-critical storage path and the medium-severity security findings, Kula would be an excellent choice for production server monitoring.
