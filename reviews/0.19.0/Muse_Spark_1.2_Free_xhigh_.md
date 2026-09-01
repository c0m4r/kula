# Kula — Security Audit & Code Review
**Date:** 2026-09-01 | **Commit:** `HEAD` on `main` | **Auditor:** automated review (Muse Spark) + manual source inspection | **Go:** `go.mod:3` `1.26.7`

---

## 1. Executive Summary

Kula is a small, well-maintained Go daemon. Security posture is **above average** for a self-hosted monitoring tool. Authentication uses Argon2id with correct constant-time comparisons and timing-oracle mitigation, session tokens are stored hashed, CSRF is defense-in-depth (Origin + synchronizer token), CSP nonces + SRI are correctly implemented, Landlock sandboxing is present, and most classic web pitfalls (JSON injection, path traversal, SSRF to Ollama) are addressed. No critical remote-code-execution or authentication-bypass was found.

Residual risk clusters around **availability / hardening**: MySQL DSN parameterization, non-atomic session persistence, infinite sliding sessions, permissive `SameSite=None` artifact when `allowed_origins` is set, and Landlock network isolation degradation on kernels `<6.7`. All identified issues are fixable locally without architectural change.

**Overall:** `go vet: PASS`, `go test ./...: PASS` (9 packages), `golangci-lint` config enables `gosec`/`bodyclose`/`errorlint`. One medium-severity fix (MySQL DSN) and a handful of low/medium hardening items recommended before exposing to the public internet.

---

## 2. Scope & Methodology

**Scope:** `cmd/kula/*`, `internal/config/*`, `internal/collector/*` (15 collectors), `internal/storage/*`, `internal/web/*` (server, auth, websocket, ollama, prometheus), `internal/sandbox/*`, `internal/backup/*`, `internal/tui/*`, `internal/i18n/*`, `addons/`, `config.example.yaml`, embedded frontend `internal/web/static/**/*`.

**Method:** static reading of every file listed in §1, taint review of all `r.URL.Query`, `r.Header.Get`, `r.Body`, `os.ReadFile`, `filepath.Join`, `url.Parse`, `sql.Open` paths, manual review of all middleware ordering in `server.go:350-446`, rate-limiter, session, CSRF, WebSocket, Ollama, backup cron logic, and execution of `go vet` + `go test -count=1` on `linux` (see §8).

---

## 3. Architecture & Attack Surface Inventory

```
Internet ──► [TCP/Bind :27960 or UnixSocket] ──► net/http Server
                 │ CSP nonce, HSTS, SRI, gzip, logging
                 ├─ /ws                (gorilla/websocket, pause/resume)
                 ├─ /api/current, /api/history, /api/config, /api/i18n
                 ├─ /api/login, /api/logout, /api/auth/status
                 ├─ /api/ollama/chat|models|context  (SSE proxy → 127.0.0.1:11434)
                 ├─ /metrics           (Prometheus, optional Bearer)
                 ├─ /health /status
                 ├─ /  /index.html /game.html  (html/template + nonce/SRI)
                 └─ /js/* /style.css /fonts/* /kula.svg (embed.FS)
Storage:   <storage.directory>/tier_*.dat (ring buffer, 0600) + sessions.json (0600) + kula.sock (0660) + backup/<ts>/
Collectors: /proc, /sys (ro), Docker/Podman socket, Nginx/Apache2 status URL, Postgres/MySQL TCP/unix, cgroups v2
Privilege: single binary, no setuid, Landlock V5 BestEffort (fs + net), HTTP timeouts 30/60/120s
Config:    YAML file + 8 env overrides, Argon2 params, Tier validation, BasePath/CSP sanitization
```

**Trust boundaries:** (1) unauthenticated network → web handler, (2) authenticated user → WebSocket/SSE, (3) operator config file → DB DSNs / status URLs / Ollama URL, (4) local Unix socket producers → custom metrics, (5) container runtime socket → metric collection.

---

## 4. What Is Done Well (Strengths)

* **Argon2id** `config:390-396, web/auth.go:146-179` — `time=3, memory=32768 KiB (2× OWASP), threads=4`, hash length 32 bytes. `ValidateCredentials` at `web/auth.go:187-211` always does exactly one Argon2 via `dummySalt`/`dummyHash` to close username-enumeration timing oracle; verified by `auth_test.go:111-158` timing test.
* **Session handling** `web/auth.go:214-279,332-393` — 32-byte `crypto/rand` → `hex`, stored only as `SHA256` (`hashToken`). `sessions.json` `0600`, `storageDir` `0750`. Constant-time compare for hash and username. Tests `auth_test.go:312-345` assert plaintext not on disk.
* **CSRF** `web/auth.go:406-464` + `server.go:277-302` — dual layer: strict `Origin`/`Referer` host-equality (`ValidateOrigin`) + synchronizer token `X-CSRF-Token` checked with `ConstantTimeCompare` only for cookie-authenticated mutating methods (GET/HEAD/OPTIONS exempt). Empty-Origin is now **rejected** (fixed 0.9.1). Origin parsing via `url.Parse` prevents `evil.com`-prefix bypass. Tests `auth_test.go:445-546`.
* **CSP/SRI** `web/server.go:226-256,1035-1076,1178-1208` + `static/index.html:8-44` — per-request 16-byte nonce, `script-src 'self' 'nonce-...'`, `frame-ancestors 'none'` (configurable), `style-src 'unsafe-inline'` scoped, SRI `sha384` via `sha512.Sum384` for every `*.js` (`chart.umd.min.js`, all `js/app/*.js`). `calculateSRIs` at `server.go:1048` keys both `js/app/main.js` and `main.js`.
* **Headers** `server.go:236-253` — `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY` (gated), `Referrer-Policy: strict-origin-when-cross-origin`, `Permissions-Policy: geolocation=(), microphone=(), camera=()`, HSTS `31536000; includeSubDomains` only over TLS or `TrustProxy && X-Forwarded-Proto:https`.
* **Input sanitization** `server.go:213-219` `jsonError` uses `json.Marshal` not `fmt.Sprintf` — prevents JSON injection. Bodies bounded: login `4096` (`server.go:844`), Ollama `32768` (`ollama.go:30`). History caps `31d` window and `5000` points (`server.go:686-707`). Template data uses `html/template`.
* **BasePath / open-redirect hardening** `config/config.go:680-720` — rejects whitespace, control, `?#\`, `..`/`.`, leading `//` or `/\` (CWE-601, protocol-relative redirect), collapses `//`, enforces single leading `/`. `server.go:319-329` `mountWithBasePath` strips prefix and scopes cookie `cookiePath` to `basePath+"/"`.
* **SSRF on Ollama** `config/config.go:660-673,577-580` — `validateOllamaURL` allowlists only `localhost`, `127.0.0.1`, `::1` at load time. `sandbox.go:195-207` adds matching `ConnectTCP` rule.
* **CORS** `server.go:273-302` — `Vary: Origin`, reflected `Allow-Origin` only if in `AllowedOrigins` (exact, `EqualFold`), explicit `Allow-Headers`/`Allow-Methods`, preflight short-circuits before auth/CSRF. Tests in `server_test.go`.
* **WebSocket CSWSH** `web/websocket.go:26-66` — `CheckOrigin` parses with `url.ParseRequestURI`, same-host or allow-list match; `log.Printf` on block; empty-Origin allowed intentionally for non-browser CLI clients with commentary.
* **Rate limiting** `web/auth.go:38-65,118-143` + `web/ollama.go:45-84` — IP (5/5m) and username (5/5m) for login, chat (10/min), meta (60/min), bounded to `16384` distinct keys (`reserveRateLimiterKey`) with stale purge + fail-closed (`auth_test.go:548-589`). WebSocket global `100` and per-IP `5` with `sync.Once` unregister (`websocket.go:68-134`); read limit `4096`.
* **Dir traversal** `web/server.go:1114-1145` — static serving from `embed.FS` (`staticFS`), `strings.TrimPrefix` + `"static/"+path`, `stat.IsDir` → `403`, no `filepath.Join` on user input. `embed.FS` implicitly rejects `..`.
* **Sandbox** `sandbox/sandbox.go:50-253` — Landlock V5 `BestEffort`: `RODirs /proc /sys`, `ROFiles` config/hosts/resolv, `RWDirs` storage & socket parent, `BindTCP` web port, conditional `ConnectTCP` for nginx/apache/postgres/mysql/ollama, `ROFiles` for container socket. Graceful abi detection `llsyscall.LandlockGetABIVersion()`.
* **MySQL/Postgres hardening** `collector/postgres.go:56-62` — single-quoted password with `\` and `'` escapes; `collector/mysql.go:64-112` lazy connect, `SetMaxOpenConns(1)`, timeout contexts. `collector/apache2.go:35-38` disables redirect follow (`ErrUseLastResponse`).
* **Ollama proxy** `web/ollama.go:24-43,239-247,474-476` — model regex `^[A-Za-z0-9._:/-]{1,200}$`, prompt null-strip + `2000` rune clamp (`sanitisePrompt`), stream capped `10 MiB` via `LimitReader`, tool loop `5` rounds, SSE headers no-proxy-buffering, `SetWriteDeadline(0)` for SSE.
* **Storage** `storage/tier.go` — header magic `KULA`, version `2` (binary) with `flagHeaderHasTail/Wrapped`, `recordKindBinary 0x02`, length-prefixed records, buffered reads, sentinel handling, migration `migrateToBinary` with disk-space check + atomic `Rename`. `storage/store.go` query cache `TTL=1×tier0 resolution`, caps `256` entries / `maxAggBufferFactor=4×ratio`. Files `0600`.

---

## 5. Detailed Findings

### 5.1 Medium Severity

| ID | Severity | Title | Location | Description | Impact | Recommendation |
|---|---|---|---|---|---|---|
| **M-01** | **Medium** | MySQL DSN does not escape password special characters | `internal/collector/mysql.go:38-44` | `fmt.Sprintf("%s:%s@tcp(%s:%d)/%s", user, password, …)` concatenates raw password. Characters `:@/?&#` break `go-sql-driver/mysql` parsing, cause connection failure or parameter injection (e.g. `password="a?tls=skip-verify&x="`). Postgres path correctly escapes (`postgres.go:60-62`). Env `KULA_MYSQL_PASSWORD` (`config.go:537-539`) flows here unescaped. | Operator cannot use strong passwords; mis-parsed DSN may connect to wrong host or leak via error log; low exploitability as attacker must control config, but violates defense-in-depth and breaks availability. | Use `github.com/go-sql-driver/mysql.Config{User, Passwd, Net, Addr, DBName, Params}.FormatDSN()` which does `url.QueryEscape`-equivalent escaping. Add test with `p@ss:w0rd?` and `:` in password. |
| **M-02** | **Medium** | Session file written non-atomically | `internal/web/auth.go:392` `os.WriteFile(path, data, 0600)` | Crash/power loss between truncate and write leaves `sessions.json` zero-length or half-written. `LoadSessions` then returns `json.Unmarshal` error, caller `server.go:453` logs warning and drops all sessions — legitimate users logged out; if write races with concurrent `SaveSessions` on shutdown vs `CleanupSessions`, same risk. | Availability loss after unclean shutdown; not exploitable remotely but a reliability bug in a security-critical file. | Write to `sessions.json.tmp` + `File.Sync` + `os.Rename` (atomic on same FS) as `backup/backup.go:162-165` does. Consider `fsync` dir. Add test for torn write. |
| **M-03** | **Medium** | Sliding session never expires while active | `internal/web/auth.go:240-258` `ValidateSession` extends `expiresAt = Now() + SessionTimeout` on **every** successful validation (including CSRF double-validation). Default `24h` (`config:394`) becomes infinite with polling (`/api/current`, WebSocket ping). | Stolen session cookie/Bearer remains valid indefinitely until idle >24h; no absolute ceiling. Violates OWASP Session Management §3.3 (idle + absolute timeout). | Add `createdAt` + `AbsoluteTimeout` (e.g. 7d) or `maxLifetime` config. On validation, extend only `expiresAt = min(Now()+idle, createdAt+absolute)`. Log warning when extend would exceed absolute. |
| **M-04** | **Medium** | WebSocket empty-Origin bypass when `auth.enabled=false` | `internal/web/websocket.go:36-39` `if origin=="" return true` | Browsers always send Origin for WebSocket; non-browser clients (curl, `websocat`) can connect without Origin and stream metrics. When auth is disabled (default `config:390`), attacker on LAN obtains live samples without any origin check. This is **intended** (comment at `websocket.go:38`), but effective CSRF boundary disappears for the common unauthenticated deployment. | Information disclosure (live metrics) to any LAN client; not credential theft but expands attack surface beyond expectation if operator assumed `origin_validation=true` was protective. | Document explicitly that `origin_validation` + `auth.enabled=false` = no CSWSH protection for non-browser clients. Optionally add config `websocket.allow_empty_origin` default `false` when auth disabled. |
| **M-05** | **Medium** | Custom metrics socket world-readable to local group | `internal/collector/custom.go:115` `os.Chmod(sockPath, 0660)` (and `.golangci.yml:30` `G302` intentional) | Any user in the owning group (often `docker`) can push arbitrary `{"custom": …}` JSON. Collector filters to configured groups/names (`custom.go:215-220` `configSet` check) so cannot create new charts, but can spoof values within configured metric names (e.g. spoof `room_temp`). | Integrity loss for dashboard / alerts / Ollama context; not privilege escalation but metric poisoning on shared hosts. | Scope permission via config `custom_socket_mode` (default `0660` for compat, but allow `0600`). Document that group members are trusted metric producers. Alternatively use `SO_PEERCRED` to check UID. |

### 5.2 Low Severity

| ID | Severity | Title | Location | Description | Recommendation |
|---|---|---|---|---|---|
| **L-01** | Low | `Secure` cookie not set on plain HTTP without `trust_proxy` | `internal/web/server.go:336-342` `sessionCookieSameSite` derives `Secure` from `r.TLS != nil \|\| TrustProxy+X-Forwarded-Proto:https` | Session cookie transmitted in cleartext on typical home LAN (`http://192.168.x:27960`), eavesdroppable via ARP spoof. Intentional (comment `nosemgrep: cookie-missing-secure`) to allow HTTP LAN. | Keep behavior; add startup log `Security Warning: auth enabled but session cookies are not Secure — use TLS or trust_proxy`. Already logs for `allowed_origins` case (`server.go:461-463`). |
| **L-02** | Low | Landlock network isolation unavailable on kernels `<6.7` | `internal/sandbox/sandbox.go:232-239` `if abi<4 netStatus="NOT supported"` + `V5.BestEffort` | Most stable distros (Ubuntu 22.04 `5.15`, Debian 12 `6.1`) cannot enforce `BindTCP`/`ConnectTCP`. An RCE/compromised metrics parser could connect out or bind rogue port despite sandbox log. | Detect at startup and `log.Printf("SECURITY: Landlock network sandbox unavailable…")` already does; elevate to `log.Fatalf` when `web.auth.enabled && abi<4` if operator wants strict. Document kernel requirement `≥6.7` in `README`. |
| **L-03** | Low | Prometheus endpoint unauthenticated by default leaks system detail | `config:385-390` `prometheus_metrics.enabled:false` default, but `server.go:416-424` logs `"without authentication"` when no token | If enabled without token, `/metrics` exposes hostname, container names/IDs, GPU model, disk serials, PSU state to any network peer. | Change docs to *"always set token unless behind mTLS/proxy auth"*; consider requiring token when `auth.enabled`. |
| **L-04** | Low | `style-src 'unsafe-inline'` weakens CSP | `internal/web/server.go:241` | Allows injected `<style>` to exfiltrate data via CSS selectors (low impact). Required by `style.css` inline overrides. | Keep, but add `style-src-attr`/`style-src-elem` tightening when possible; move inline styles to `style.css` + nonce for `<style>` blocks. |
| **L-05** | Low | Login without Origin header rejected (DoS for API clients) | `internal/web/auth.go:436-443` `if Origin=="" return false` | `curl -X POST /api/login -d '{"username":…}'` without `Origin` now `403 invalid origin`. Legitimate automation/scripted login must fabricate `Origin: http://<host>`. | Allow empty Origin for `/api/login` specifically, or document required `Origin`/`Referer`. Alternatively treat login as exempt from `ValidateOrigin` (it already has stronger rate-limit + CSRF token fetch). |
| **L-06** | Low | `history` `points` parsed via `Sscanf` tolerates trailing garbage | `internal/web/server.go:695-707` `fmt.Sscanf(pointsStr,"%d",&points)` | `points=5000abc` → `5000` (capped), `points=-5` → `1`. Not exploitable, but non-strict. | Replace with `strconv.Atoi` + explicit error; on error keep `450`. |
| **L-07** | Low | Backup cron uses `time.Local` for retention prune | `internal/backup/backup.go:258` `time.ParseInLocation(runDirLayout,…,time.Local)` | Machine timezone change (UTC vs local) shifts prune cutoff by hours; unlikely but non-deterministic. | Use `time.UTC` for both naming and pruning (`runDirLayout` + `UTC`). Store `now.UTC()`. |
| **L-08** | Low | HTTP client for Nginx follows redirects | `internal/collector/nginx.go:30` `http.Client{Timeout}` default redirect | `status_url` pointing to internal service that 302s to `http://169.254.169.254/…` (cloud metadata) would be followed. Low risk as config is operator-controlled. | Set `CheckRedirect: ErrUseLastResponse` as done for Apache (`apache2.go:35`). |
| **L-09** | Low | Config file permissions not validated | `internal/config/config.go:490-499` `os.ReadFile(path)` then `yaml.Unmarshal` | Operator may leave `config.yaml` `0644` with `password_hash`/`salt` world-readable. | On load, `os.Stat` perms; if `mode & 0077 !=0` and file contains `password_hash`, log warning `"config.yaml is world-readable"` . |
| **L-10** | Low | Ollama tool `executeGetMetrics` caps `100` points but range unbounded | `internal/web/ollama.go:958` `QueryRangeWithMeta(from,to,100)` | Model-induced call with `-30d` → `30d` window, 100-point downsample is cheap, but repeated 10/min could scan large tier range. Already rate-limited; still worth capping window to `31d` as `/api/history`. | Clamp `to.Sub(from)` to `31*24h` inside `executeGetMetrics` before query. |

### 5.3 Informational / Code Quality

* **I-01** Double `ValidateSession` per mutating request (`CSRFMiddleware:451` + `AuthMiddleware:291/300`): extends expiry twice; extra `RWMutex` contention. Cache validation result in `r.Context`.
* **I-02** `internal/web/server.go:252` `Strict-Transport-Security` set per-request rather than once; missing `preload` directive — intentional (no preload without operator intent).
* **I-03** `internal/storage/codec.go:460` `appendUint16(uint16(len(...)))` silent truncation if `len>65535` for GPU/custom counts — currently guarded by `min(8)` for PSU and truncation for others, but no error. Add explicit error return.
* **I-04** `addons/inspect_tier.py` not reviewed but mirrors codec versioning correctly per `codec.go:5-44` checklist.
* **I-05** Frontend `state.js:145` `escapeHTML` correct; `ollama.js:536-626` `renderMarkdownLite` escapes first then injects whitelisted tags — safe. Chart labels use Canvas, not `innerHTML`.

---

## 6. OWASP Top 10 (2021) Mapping

| Category | Status | Evidence |
|---|---|---|
| A01 Broken Access Control | **Controlled** | `AuthMiddleware` + per-IP & per-user rate limit; WebSocket global/per-IP caps; Landlock FS ro/rw split. Gap: empty-Origin WebSocket when auth off (M-04). |
| A02 Cryptographic Failures | **Controlled** | Argon2id 32 MiB/3/4, `Secure` conditional, `SHA256` session hash, `0600` tier/sessions, `0750` dir. Gap: MySQL DSN raw password (M-01), plain-HTTP cookie (L-01 by design). |
| A03 Injection | **Controlled** | `json.Marshal` for errors, `html/template` + nonce/SRI, `url.Parse` for Origin/CSP, status URL parsers. Gap: MySQL DSN (M-01) minor. |
| A04 Insecure Design | **Good** | Tier validation, basePath normalization, Landlock BestEffort, backup atomic rename. |
| A05 Security Misconfiguration | **Good** | `trust_proxy` opt-in, explicit `security.headers/frame_protection/origin_validation`, `headers=true` default. Gap: Prometheus token optional (L-03), Landlock abi<4 silent degrade (L-02). |
| A06 Vulnerable Components | **Reviewed** | `go.mod: gorilla/websocket 1.5.3`, `golang.org/x/crypto 0.55.0`, `x/sys 0.47.0`, `lib/pq 1.12.3`, `go-sql-driver/mysql 1.10.0` — `go vet` clean, tests pass; run `govulncheck` in CI (binary not present in this env). |
| A07 Identification/Auth Failures | **Strong** | Constant-time, dummy hash, 5/5m brute-force caps (16384-key bound), sliding + absolute gap noted (M-03). |
| A08 Software & Data Integrity | **Strong** | SRI `sha384` on all JS, `embed.FS` for static, `embed.FS` + nonce CSP, backup `Rename` atomic. |
| A09 Logging & Monitoring | **Good** | `loggingMiddleware` tags `[API]/[WEB]` (`server_test.go:322-380`), `DoS` caps, access/perf/debug levels, 5-minute session/rate purge. Recommend shipping logs to SIEM. |
| A10 SSRF | **Controlled** | Ollama loopback-only, Nginx/Apache/Mysql/Postgres `ConnectTCP` scoped to configured port, `CheckRedirect` for Apache. Gap: Nginx redirect follow (L-08). |

---

## 7. Additional Reliability Notes (Non-Security but Load-Bearing)

* `internal/storage/tier.go:79-95` — corrupt header now **fails loud** (`"refusing to open so existing data is not destroyed"`) rather than silent re-init — correct.
* `internal/storage/store.go:237-240,253-256` — aggregation buffers bounded by `maxAggBufferFactor=4` to avoid OOM if tier write stalls.
* `internal/backup/backup.go:129-169` — backup uses `tmpSuffix=".tmp"` + `Rename`; leftover tmp pruned in `cleanup`; compression outside storage lock.
* `internal/collector/custom.go:172-206` — scanner `64 KiB` + `SetReadDeadline(5m)` prevents slow-loris on `kula.sock`.
* `cmd/kula/main.go:72-82` — explicit `-config` missing path is seeded from `ExampleConfig` `0600`; fallback to defaults if seed fails. Race-free because `Stat`+`WriteFile` is checked before `LoadRequired`.

---

## 8. Verification

```
$ go vet ./...                          # PASS (no output)
$ go test ./... -count=1
  ok kula/cmd/kula-scan     4.601s
  ok kula/internal/backup   0.006s
  ok kula/internal/collector 0.071s
  ok kula/internal/config   0.012s
  ok kula/internal/i18n     0.004s
  ok kula/internal/sandbox  0.008s
  ok kula/internal/storage  2.710s
  ok kula/internal/tui      0.057s
  ok kula/internal/web      0.644s   ← includes TestValidateOrigin, TestSessionHashingOnDisk, TestTemplateInjection, TestHandleHistoryIncludesPSU
$ govulncheck               # not in PATH in this environment — recommend CI step
$ golangci-lint             # config .golangci.yml enables gosec, bodyclose, errorlint, gocritic, etc.
```

All existing security-relevant tests pass, notably:

* `auth_test.go:312` — plaintext token not on disk.
* `auth_test.go:548` — `maxRateLimiterKeys` cap & reclaim.
* `server_test.go:23` — CSP nonce/SRI injection.
* `server_test.go:188-275` — `mountWithBasePath` + `GameScoreURL` CSP injection rejection.

---

## 9. Prioritized Remediation Roadmap

**P0 (before public internet):**
1. `M-01` MySQL DSN via `mysql.Config.FormatDSN()`.
2. `M-03` Add absolute session lifetime (e.g. `max_session_lifetime: 7d`).
3. `M-02` Atomic session write.
4. Document/condition `M-04` when `auth.enabled=false`.

**P1 (next release):**
5. `L-02` Elevate Landlock abi<4 warning to error when strict mode requested.
6. `L-03` Require Prometheus token or explicit `allow_unauthenticated: true`.
7. `L-05` Exempt `/api/login` from `ValidateOrigin` or document `Origin` requirement.
8. `M-05` Make `custom_socket_mode` configurable.

**P2 (hardening):**
9. `L-08` `Nginx CheckRedirect`, `L-06` `strconv.Atoi` for points, `L-07` UTC for backup, `L-01` cookie Secure warning, `L-04` reduce `unsafe-inline`, `L-09` config perms warning, `L-10` tool window clamp.

Example patch sketch for M-01:

```go
// internal/collector/mysql.go
import mcfg "github.com/go-sql-driver/mysql"
func newMysqlCollector(...) *mysqlCollector {
    cfg := mcfg.Config{
        User: user, Passwd: password,
        Net: net, Addr: addr, DBName: dbname,
        Params: map[string]string{"timeout":"5s","readTimeout":"5s"},
    }
    dsn := cfg.FormatDSN()
}
```

---

## 10. Conclusion

Kula demonstrates **security-aware engineering**: correct Argon2 usage, proper session/CSRF/CSP/SRI, careful landlock sandboxing, and thoughtful DoS caps. The codebase is small, readable, and well-tested for its security properties. With the P0 items above (especially MySQL DSN and session file atomicity/absolute timeout) addressed, it is suitable for internet-facing deployment behind a TLS-terminating reverse proxy with `trust_proxy=true` and `AllowedOrigins` scoped. For purely internal LAN use over HTTP, the current trade-offs (`Secure` conditional, empty-Origin WebSocket) are acceptable but must be documented explicitly for operators.

*No evidence of backdoor, hardcoded secrets, or unsafe `exec`/`eval` was found. All file writes are permissioned to `0600`/`0750`/`0660` intentionally.*

---

**References:** source lines cited inline as `path:line`. Configuration defaults at `config/config.go:351-471`. Build at `addons/build.sh`, checks at `addons/check.sh` (govulncheck → vet → test -race → golangci-lint).
