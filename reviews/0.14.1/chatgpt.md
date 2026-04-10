# Code review: `c0m4r/kula`

I reviewed the main branch code paths that matter most for a self-hosted monitoring daemon: `internal/web/auth.go`, `internal/web/server.go`, `internal/web/websocket.go`, `internal/storage/store.go`, `internal/config/config.go`, plus the repo metadata and example config. The project already shows a lot of defensive engineering, especially in the web layer and storage tiering. ([GitHub][1])

## Scores

Security: **7.5/10**. The app has real hardening work in place, including CSP nonce generation, HSTS, SameSiteStrictMode cookies, Argon2id password hashing, CSRF token checks, WebSocket origin/limit controls, and storage permission handling. The score is held back by the metrics-auth wiring gap and by the relatively weak login throttling model. ([GitHub][2])

Performance: **8.6/10**. The storage engine is clearly built for throughput and low latency, with an O(1) latest-sample cache, a query cache for repeated range lookups, tier ratio validation at config load time, and a startup warm-up path to avoid the first full disk scan. ([GitHub][3])

Code quality: **7.8/10**. The code is modular and intentional, but there is some duplicated path logic, a few places where configuration and runtime path handling diverge, and a lot of security behavior is spread across middleware, handlers, and config in ways that will reward careful maintenance. ([GitHub][3])

Overall: **7.9/10**. This is a serious, unusually well-defended monitoring tool, but it still has at least one high-priority security verification point and a couple of medium-severity improvements that would make it much harder to misuse in production. ([GitHub][2])

## What is already solid

The web layer has a strong baseline: a per-request CSP nonce, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy`, `Permissions-Policy`, and conditional HSTS when the connection is TLS or a trusted proxy says it is HTTPS. That is a very respectable defense stack for a dashboard that serves dynamic JS and a real-time UI. ([GitHub][2])

Authentication is also reasonably mature. Passwords use Argon2id, credential checks use constant-time comparison, sessions are stored hashed at rest, cookies are `HttpOnly` and `SameSite=Strict`, and CSRF defense combines Origin/Referer validation with a synchronizer token. ([GitHub][4])

WebSocket handling has been actively hardened. The upgrader checks origin, connection counts are capped globally and per IP, the read limit is set to 4096 bytes, and unregister logic uses `sync.Once` to avoid double cleanup. That is exactly the sort of lint-free fencing you want around a long-lived socket. ([GitHub][5])

The storage path is optimized rather than naive. The store warms the latest-sample cache at startup, caches repeated history queries, and validates tier ratios so aggregation does not silently drift into memory abuse territory. The config layer also constrains tier structure and storage permissions before the server starts. ([GitHub][3])

## Findings

### 1) High: Prometheus metrics auth appears to be advertised but not enforced in the route wiring

The config and startup log explicitly describe an optional bearer token for `/metrics`, but the route registration shown in `Start()` mounts `/metrics` only with logging middleware. I did not see token enforcement in the router path I reviewed, so unless `handleMetrics()` itself re-checks the token internally, this endpoint is effectively public even when the config suggests it is protected. That is a sharp edge, because metrics often contain hostnames, service names, cardinality-heavy telemetry, and environment hints that you may not want to expose broadly. ([GitHub][6])

Recommended fix:

```go
metricsHandler := http.HandlerFunc(s.handleMetrics)

if token := s.cfg.PrometheusMetrics.Token; token != "" {
    metricsHandler = requireBearerToken(token, metricsHandler)
}

mux.Handle("/metrics", loggingMiddleware(s.cfg, metricsHandler))
```

A small middleware like this makes the protection explicit, testable, and hard to accidentally bypass later. ([GitHub][2])

### 2) Medium: Login throttling is only IP-based

The login limiter allows five attempts per five minutes per IP. That is good as a first fence, but it is easy to sidestep with distributed sources, shared NATs, or a botnet that rotates addresses. For a monitoring UI that can expose operational data, credential-stuffing resistance should not depend on a single IP bucket. ([GitHub][7])

Recommended fix:

```go
key := ip + ":" + strings.ToLower(username)

if !s.auth.Limiter.Allow(key) {
    http.Error(w, `{"error":"too many attempts"}`, http.StatusTooManyRequests)
    return
}
```

Keep the existing per-IP limit, then add a username-based limit and exponential backoff. That way, one noisy client cannot keep hammering the same account through a parade of source IPs. ([GitHub][7])

### 3) Medium: Storage directory handling is inconsistent

`NewStore()` computes `absDir` and creates that directory, but then it opens tier files with `filepath.Join(cfg.Directory, ...)` instead of `absDir`. Meanwhile, the config layer may rewrite the storage directory from `/var/lib/kula` to a home-directory fallback, and it also accepts `KULA_DIRECTORY`. That combination makes the path story more fragile than it needs to be, especially if someone supplies a relative directory or changes the working directory at runtime. ([GitHub][8])

Recommended fix:

```go
tierPath := filepath.Join(absDir, fmt.Sprintf("tier_%d.dat", i))
tier, err := OpenTier(tierPath, tc.MaxBytes)
```

Using the normalized path everywhere keeps the storage root singular and predictable. It is a small change with a large “fewer ghost files in the attic” payoff. ([GitHub][3])

## Code-quality and performance notes

The good news first: the code is not accidentally expensive in the obvious places. The storage layer caches the latest sample, deduplicates repeated history queries, and validates tier ratios so the aggregation pipeline stays bounded. The web server also sets sane timeouts and uses WebSocket limits to avoid easy resource exhaustion. ([GitHub][3])

The main quality wart is that configuration, middleware, and storage behavior are tightly coupled. That is manageable in a small project, but it means regressions will tend to arrive as “tiny” changes with long shadows. Tests around the security-sensitive edges, especially metrics auth, CSRF, and storage path resolution, would pay for themselves quickly. The repo already has tests and a changelog culture, which makes that a very natural next step. ([GitHub][4])

## Overall summary

Kula looks like a project that has been built with real operational intent rather than hobby fog. The strongest parts are the layered web hardening, the CSRF and session handling, and the performance-conscious storage engine. The biggest security concern is the mismatch between the stated Prometheus token protection and the route wiring I could verify, followed by the weak brute-force model on login. Tighten those, normalize the storage path usage, and the codebase moves from “pretty solid” to “I would trust this on a server that matters.” ([GitHub][2])

[1]: https://github.com/c0m4r/kula "GitHub - c0m4r/kula: Lightweight, self-contained Linux® server monitoring tool · GitHub"
[2]: https://github.com/c0m4r/kula/blob/main/internal/web/server.go "kula/internal/web/server.go at main · c0m4r/kula · GitHub"
[3]: https://github.com/c0m4r/kula/blob/main/internal/storage/store.go "kula/internal/storage/store.go at main · c0m4r/kula · GitHub"
[4]: https://github.com/c0m4r/kula/blob/main/internal/web/auth.go "kula/internal/web/auth.go at main · c0m4r/kula · GitHub"
[5]: https://github.com/c0m4r/kula/blob/main/internal/web/websocket.go "kula/internal/web/websocket.go at main · c0m4r/kula · GitHub"
[6]: https://github.com/c0m4r/kula/blob/main/config.example.yaml "kula/config.example.yaml at main · c0m4r/kula · GitHub"
[7]: https://github.com/c0m4r/kula/raw/refs/heads/main/internal/web/auth.go "raw.githubusercontent.com"
[8]: https://github.com/c0m4r/kula/blob/main/internal/config/config.go "kula/internal/config/config.go at main · c0m4r/kula · GitHub"
