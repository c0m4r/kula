Note: This review was interrupted multiple times with:

```
ⓘ This content can't be shown
  We take extra caution with cybersecurity requests. If you’re a security professional, you may be able to apply for Trusted Access.
  Trusted Access: https://chatgpt.com/cyber/
  Learn more: https://help.openai.com/en/articles/20001326
```

# Kula 0.19.0 — Security and Code Review (Interim)

This document records the review evidence collected before the requested cutoff. It is
an engineering review of the local source tree, not a live assessment of a deployed
host.

| Item | Value |
|---|---|
| Review date | 2026-09-01 |
| Target | Kula 0.19.0, branch `main` |
| Commit | `6435f97` |
| Worktree at review start | Clean |
| Primary language | Go, with an embedded JavaScript dashboard |
| Scoring | CVSS v3.1 base score; deployment preconditions are stated separately |

## Executive summary

Kula has a generally strong defensive baseline: its authentication primitives, CSRF
checks, browser security headers, input-size limits, service timeouts, storage decoder
bounds, Landlock integration, and CI controls are materially better than is typical for
a small self-contained monitoring daemon. The prescribed test suite is green.

The main risk is the interaction between a remote-by-default listener and authentication
being disabled by default. A second concrete defect is a WebSocket lifecycle race that
can terminate the Go process. The release/install path and optional remote database
collectors also need security work.

This interim review records **10 security findings**:

- **3 High**
- **6 Medium**
- **1 Low**

It also records several reliability and hardening findings that are not meaningfully
scored with CVSS.

## Findings overview

| ID | Severity | CVSS v3.1 | Finding | Default exposure |
|---|---:|---:|---|---|
| KUL-SEC-001 | High | 7.5 | Web UI and telemetry API bind globally without authentication | Yes |
| KUL-SEC-002 | High | 7.5 | Recommended and Ansible installation paths lack independent artifact authenticity | Install-time |
| KUL-SEC-003 | High | 7.1 | Optional remote database monitoring permits plaintext credentials/traffic | Feature-dependent |
| KUL-SEC-004 | Medium | 6.6 | Writable storage directory crosses session and file trust boundaries | Misconfiguration-dependent |
| KUL-SEC-005 | Medium | 5.9 | WebSocket teardown can leave a closed channel in the broadcast set | Yes |
| KUL-SEC-006 | Medium | 5.5 | Custom-metrics socket has no connection concurrency limit | Feature-dependent, local |
| KUL-SEC-007 | Medium | 5.3 | Historical queries can monopolize storage access | Yes |
| KUL-SEC-008 | Medium | 5.3 | `trust_proxy` accepts forwarding headers from untrusted peers | Configuration-dependent |
| KUL-SEC-009 | Medium | 4.4 | A short container ID from a socket API panics the process | Local upstream-dependent |
| KUL-SEC-010 | Low | 3.3 | Redirects weaken loopback-only upstream URL enforcement | Feature/upstream-dependent |

---

## KUL-SEC-001 — Global unauthenticated monitoring endpoint by default

- **Severity:** High
- **CVSS:** 7.5 — `CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:N/A:N`
- **CWE:** CWE-306 (Missing Authentication for Critical Function), CWE-284

### Evidence

- `internal/config/config.go:378-397` enables the web UI, leaves `Listen` empty,
  and leaves authentication disabled through its zero-value `Enabled` field.
- `internal/web/server.go:516-548` defines an empty listener as both
  `0.0.0.0:<port>` and `[::]:<port>`.
- `addons/docker/Dockerfile:30-34` explicitly rewrites the packaged configuration
  to `0.0.0.0`.
- `addons/docker/docker-compose.yml:5-6` uses host PID and host network namespaces.
- The current sample, history, configuration metadata, and WebSocket feed are protected
  only when authentication is enabled in the middleware configuration.

### Impact

Any network peer that can reach the port can read the full monitoring view. Depending
on enabled collectors, this can reveal host identity, kernel/system details, filesystem
layout, interfaces, container names and resource data, database health, and custom
operational metrics. A host firewall or an explicitly loopback-only configuration
reduces the practical exposure, but neither is guaranteed by Kula's defaults.

### Recommendation

1. Default `web.listen` to `127.0.0.1` (and optionally `::1`).
2. Refuse a wildcard/non-loopback listener when authentication is disabled unless the
   operator sets an explicit `allow_unauthenticated_remote: true` acknowledgement.
3. Make Docker and Ansible examples auth-enabled or loopback/reverse-proxy-only.
4. Log a prominent startup warning containing the actual bound addresses and auth state.
5. Add tests for the effective packaged defaults, not only `DefaultConfig()`.

---

## KUL-SEC-002 — Installation authenticity is not independently established

- **Severity:** High
- **CVSS:** 7.5 — `CVSS:3.1/AV:N/AC:H/PR:N/UI:R/S:U/C:H/I:H/A:H`
- **CWE:** CWE-494 (Download of Code Without Integrity Check)

### Evidence

- `README.md:143-147` and `landing/index.html:144-146` recommend executing an
  installer directly from mutable branch `main`.
- `addons/install_v2.sh:160-221` correctly compares release files with
  `CHECKSUMS.sha256.txt`, but the checksum file is downloaded from the same release and
  is not authenticated by an independent signature or transparency proof.
- `--skip-verify` intentionally bypasses the checksum control.
- `addons/ansible/populate_files.sh:4-5` downloads hard-coded version **0.16.0** while
  the reviewed tree is 0.19.0, and does not verify a digest.
- `addons/ansible/roles/kula/tasks/main.yaml:21-25` installs RPMs with
  `disable_gpg_check: true` as root.

### Impact

If the mutable installer path, release publication account, or downloaded Ansible
artifact is replaced, the resulting code is commonly executed with administrative
privileges. HTTPS and same-release SHA-256 verification protect against accidental
corruption, but they do not provide an independent publisher-authenticity boundary.
This is an install-time supply-chain risk, not evidence that current release files are
malicious.

### Recommendation

1. Publish signed checksums or Sigstore attestations and verify them before installing.
2. Point documentation at an immutable version/tag and show a pinned installer digest.
3. Remove the stale Ansible version; make the version and expected digest explicit
   inputs and fail closed on mismatch.
4. Do not disable RPM signature verification. Prefer a signed package repository.
5. Retain `--skip-verify` only as an unmistakably unsafe manual recovery option.

---

## KUL-SEC-003 — Optional database collectors can send secrets in plaintext

- **Severity:** High (when a collector targets a non-local database)
- **CVSS:** 7.1 — `CVSS:3.1/AV:A/AC:L/PR:N/UI:N/S:U/C:H/I:L/A:N`
- **CWE:** CWE-319 (Cleartext Transmission of Sensitive Information)

### Evidence

- PostgreSQL defaults to `sslmode: disable` at `internal/config/config.go:445-452`.
- `internal/collector/postgres.go:46-63` passes the configured SSL mode through to
  lib/pq; a secure mode is possible, but is not the default.
- `internal/collector/mysql.go:38-45` constructs a raw DSN with no TLS option and the
  configuration exposes no CA, server-name, or client-certificate settings.
- The MySQL DSN is assembled with `fmt.Sprintf`; driver-specific escaping and option
  construction are not delegated to `mysql.Config.FormatDSN()`.

### Impact

For loopback or Unix-socket databases this is generally not a network exposure. For a
remote database, an adjacent network observer can potentially recover monitoring
credentials and observe or alter collected database traffic. The collectors are
disabled by default, so this finding applies only when configured.

### Recommendation

1. Add explicit TLS configuration for MySQL and use `mysql.Config`/`FormatDSN()`.
2. Default PostgreSQL remote connections to `verify-full`; retain `disable` only for
   Unix sockets or an explicit unsafe override.
3. Support CA roots, server name, and optional client certificates for both drivers.
4. Reject non-loopback TCP database targets without verified TLS, or emit a high-signal
   startup warning when the operator explicitly permits them.

---

## KUL-SEC-004 — Storage-directory write access crosses authentication/file boundaries

- **Severity:** Medium
- **CVSS:** 6.6 — `CVSS:3.1/AV:L/AC:L/PR:L/UI:N/S:U/C:L/I:H/A:L`
- **CWE:** CWE-59, CWE-732, CWE-922

### Evidence

- `internal/config/config.go:856-872` creates and checks the special default directory,
  but does not validate ownership, existing mode, or symlinks for a custom directory.
- `internal/storage/tier.go:63-72` opens predictable tier paths with `os.OpenFile` and
  follows symbolic links.
- `internal/web/auth.go:332-365` loads `sessions.json` records as live sessions when
  their stored expiry is in the future. The stored token field is already the lookup
  hash.
- `internal/web/auth.go:367-392` rewrites the predictable session path with
  `os.WriteFile`, which is non-atomic and follows a final-component symlink.

### Impact

This requires a local account or process that can write the configured storage
directory; the packaged `/var/lib/kula` permissions normally prevent it. If that
boundary is weakened, a writer can plant the hash of a chosen session token for the
next restart and can redirect predictable writes to files writable by the Kula service
account. Concurrent shutdown or interruption can also leave a truncated session file.

### Recommendation

1. On startup, resolve the directory once and reject a symlink, unexpected owner, or
   group/world-writable directory unless explicitly acknowledged.
2. Use `openat2` restrictions where available, or directory-FD-relative opens with
   `O_NOFOLLOW`/equivalent checks for predictable files.
3. Save sessions through a mode-0600 temporary file in the verified directory, `fsync`
   it, rename atomically, then `fsync` the directory.
4. Bound `sessions.json` size and validate token-hash length, CSRF token format,
   timestamps, and configured usernames before accepting a record.
5. Document the storage directory as a security-sensitive boundary, not only a data
   directory.

---

## KUL-SEC-005 — WebSocket teardown can panic the process

- **Severity:** Medium
- **CVSS:** 5.9 — `CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:U/C:N/I:N/A:H`
- **CWE:** CWE-362 (Concurrent Execution Using Shared Resource), CWE-248

### Evidence

- `internal/web/websocket.go:117-133` sends a client to a buffered unregister channel
  and immediately closes `client.sendCh`.
- `internal/web/server.go:994-1005` removes that client asynchronously in the hub
  goroutine.
- Until removal completes, `internal/web/server.go:1017-1031` can select a send to the
  closed channel. A send to a closed Go channel panics even when it appears inside a
  `select`.
- Registration and unregistration use separate buffered channels, so under scheduling
  pressure the hub can also observe their events out of lifecycle order.

### Validation

A temporary, local regression test recreated the exact intermediate state—registered
client, queued unregistration, then closed send channel—and confirmed that the next
broadcast panics. The test passed under `go test` because it asserted/recovered that
panic. The review-only test file was then removed. No remote target was contacted.

### Impact

An unhandled panic in any goroutine terminates the daemon. With the default
unauthenticated listener, repeated connection churn can increase the likelihood of
hitting the race around a metrics broadcast. The CVSS attack complexity is High because
the externally visible failure depends on timing.

### Recommendation

Make the hub the sole owner of client lifecycle and channel closure. It should remove a
client from the map under its lock before closing the channel. Prefer one ordered event
stream or a registration acknowledgement so register/unregister cannot be reordered.
Add a churn regression test with concurrent broadcasts and run it with `-race`.

---

## KUL-SEC-006 — Custom metric socket permits unbounded concurrent handlers

- **Severity:** Medium
- **CVSS:** 5.5 — `CVSS:3.1/AV:L/AC:L/PR:L/UI:N/S:U/C:N/I:N/A:H`
- **CWE:** CWE-400 (Uncontrolled Resource Consumption)

### Evidence

- `internal/collector/custom.go:109-117` creates a group-writable `0660` Unix socket.
- `internal/collector/custom.go:142-163` starts one goroutine for every accepted
  connection without a semaphore or total connection limit.
- `internal/collector/custom.go:166-200` permits a connection to remain active, with a
  five-minute idle deadline (`customConnIdleTimeout`).
- The 64 KiB scanner limit bounds each message, but not aggregate file descriptors or
  goroutine count.

### Impact

A local principal authorized to use the socket can hold enough idle connections to
consume the daemon's descriptor limit and memory, interfering with monitoring and the
web service. The finding is local and applies only when custom metrics are enabled.

### Recommendation

Use a bounded semaphore before starting a handler, close excess connections, expose a
small configurable cap, and consider closing after one accepted message (matching the
documented one-message producer protocol). Lower the idle timeout unless persistent
producers are a supported requirement.

---

## KUL-SEC-007 — Historical queries can delay metric persistence

- **Severity:** Medium
- **CVSS:** 5.3 — `CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:L`
- **CWE:** CWE-400

### Evidence

- `internal/web/server.go:659-727` permits up to 31-day requests and caps returned
  points, but has no history-query concurrency limit or dedicated rate limiter.
- `internal/storage/store.go:314-316` holds the store-wide read lock for the entire
  query.
- `internal/storage/store.go:393-428` reads the selected tier into memory before
  downsampling it.
- `internal/storage/store.go:201-204` requires the same store's write lock to persist
  each live sample.
- The short-lived cache has a bounded size, but distinct time ranges/point counts create
  distinct keys and still perform storage reads.

### Impact

Concurrent expensive history requests can delay live writes and cause avoidable disk,
CPU, and allocation pressure. On the default unauthenticated listener this is reachable
without credentials. Existing time-range and output-point limits constrain the impact,
so availability is scored Low rather than High.

### Recommendation

1. Add a small global/per-client semaphore for historical queries.
2. Snapshot immutable tier/config references under the store lock, release it, then use
   the tier's own lock for I/O so live writes are not blocked by the full query.
3. Add a maximum raw-record/byte budget and honor request-context cancellation during
   scans and aggregation.
4. Normalize cache keys and rate-limit cache misses.

---

## KUL-SEC-008 — Proxy headers are trusted without a trusted-proxy boundary

- **Severity:** Medium
- **CVSS:** 5.3 — `CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:L`
- **CWE:** CWE-345 (Insufficient Verification of Data Authenticity)

### Evidence

`internal/web/server.go:1078-1094` uses the rightmost `X-Forwarded-For` value whenever
`trust_proxy` is true, without first checking that `RemoteAddr` belongs to a configured
proxy. The same derived address is used for login limiting, Ollama limiting, WebSocket
per-IP limits, and access logging. Forwarded protocol is likewise used for secure-cookie
and HSTS decisions when proxy trust is enabled.

### Impact

If Kula remains directly reachable while `trust_proxy` is enabled, a direct client can
choose the forwarding value, rotate the per-IP rate-limit key, evade per-IP WebSocket
quotas, and falsify audit log attribution. The separate username login limiter still
provides a meaningful brute-force control, limiting the impact.

### Recommendation

Replace the boolean with trusted proxy CIDRs or Unix-socket-only trust. Parse values with
`net/netip`, reject malformed entries, and strip the exact number of hops added by the
trusted proxy. Ignore all forwarding headers from any other immediate peer.

---

## KUL-SEC-009 — Short container IDs cause an unchecked slice panic

- **Severity:** Medium
- **CVSS:** 4.4 — `CVSS:3.1/AV:L/AC:L/PR:H/UI:N/S:U/C:N/I:N/A:H`
- **CWE:** CWE-129, CWE-248

### Evidence

After decoding the Docker/Podman-compatible `/containers/json` response,
`internal/collector/containers.go:278-295` evaluates `c.ID[:12]` without checking the
length. A syntactically valid JSON object with a running state and an ID shorter than
12 bytes therefore panics the collector process.

### Impact

The standard Docker daemon returns long IDs. Reaching this condition requires a buggy,
malicious, or substituted local socket API; control of a Docker socket is usually
already highly privileged. The resulting unhandled panic nevertheless terminates Kula.

### Recommendation

Validate that IDs are non-empty and meet the expected format before slicing. Use the
full validated ID for API requests and derive a display ID with a length-safe helper.
Add malformed-response tests. Consider disabling container auto-detection by default
or supporting a narrowly restricted read-only socket proxy.

---

## KUL-SEC-010 — Redirects weaken loopback-only upstream enforcement

- **Severity:** Low
- **CVSS:** 3.3 — `CVSS:3.1/AV:L/AC:L/PR:L/UI:N/S:U/C:L/I:N/A:N`
- **CWE:** CWE-918 (Server-Side Request Forgery)

### Evidence

- `internal/config/config.go:658-672` validates only the originally configured Ollama
  hostname against three textual loopback forms.
- The clients at `internal/web/ollama.go:448`, `:552`, `:734`, and `:776` use Go's
  default redirect behavior, which does not reapply Kula's loopback validation.
- `internal/collector/nginx.go:28-33` also follows redirects. By contrast,
  `internal/collector/apache2.go:31-38` explicitly stops them.
- Landlock is best-effort and its network rules are port-based, not destination-based,
  so it is not a complete redirect guard.

### Impact

A local upstream that can issue a redirect may cause Kula to connect to a destination
that the original URL validator would reject. This requires control or compromise of an
already trusted local service and optional integrations are disabled by default, so the
base severity is Low.

### Recommendation

Disable redirects for monitoring/AI upstreams or revalidate every redirect destination.
Resolve hostnames and require every selected address to be loopback; restrict schemes
to `http`/`https`, reject userinfo, and validate ports. Apply one shared hardened client
policy to Nginx, Apache, and Ollama.

---

## Reliability and hardening observations

These items are important, but CVSS is not a good fit because they are primarily
configuration safety, correctness, or local robustness issues.

### R-001 — Config permits more tiers than the writer implements

`internal/config/config.go:769-792` validates any number of monotonically increasing
tiers. `internal/storage/store.go:108-120` computes only two aggregation ratios and
`internal/storage/store.go:230-266` writes only tiers 0, 1, and 2. Tier 3 and later are
created and queried but never populated. Either generalize aggregation to all tiers or
reject configurations with more than three tiers.

### R-002 — Configuration parsing is permissive at security-sensitive defaults

`internal/config/config.go:487-499` uses `yaml.Unmarshal`, so unknown keys are silently
ignored. A typo in `auth.enabled` or `web.listen` can therefore retain the unsafe
default without a startup error. Use `yaml.Decoder.KnownFields(true)` and validate port
ranges, listener combinations, authentication credentials, Argon2 resource bounds,
logging values, graph bounds, and application endpoints.

### R-003 — Explicit missing config behavior contradicts fail-closed comments

`cmd/kula/main.go:60-82` says an explicit missing config must abort, but instead seeds
the path from the embedded example and falls back to built-in defaults if it cannot be
written. Because those defaults are remote and unauthenticated, a mistyped service path
can produce a running daemon with a materially different security posture. An explicit
path should fail closed; config generation should be a separate command.

### R-004 — On-disk metadata needs stronger corruption bounds

`internal/storage/tier.go:119-160` trusts several header values after only partial
validation. Record lengths are checked during scans, but a corrupt local tier can still
drive large reads/allocations. Some codec sections preallocate directly from uint16
counts (containers and custom metrics), unlike the capped PSU allocation. Validate the
header against actual file size and configured maximum, cap collection preallocations,
and add corrupt-file tests/fuzz seeds.

### R-005 — Invalid localStorage JSON can prevent dashboard startup

`internal/web/static/js/app/state.js:36` and `:61-65` call `JSON.parse` during module
initialization without a fallback. Corrupt or manually edited site storage can stop the
SPA before it can repair itself. Parse through a small safe helper and reset invalid
preferences.

### R-006 — Debug AI logging contains full conversations and metric context

`internal/web/ollama.go:438-440` and `:542-545` log the complete AI request at debug
level. This can include user-entered text, recent conversation history, and system
metric context. Treat debug logs as sensitive, document this clearly, and prefer field
metadata or opt-in redacted tracing.

### R-007 — Packaging documentation does not consistently describe effective binding

The runtime meaning of `listen: ""` is wildcard dual-stack, while some packaging and
operator-facing material describes localhost behavior. Generate deployment examples
from a single tested source of truth and add packaging smoke tests that inspect the
actual bound addresses and auth state.

## Controls that held up well

- Argon2id password hashes use random salts; comparisons and bearer-token checks use
  constant-time primitives.
- Session tokens and CSRF tokens use `crypto/rand`; persisted session tokens are hashed.
- Origin/Referer validation and synchronizer CSRF tokens protect state-changing browser
  requests when authentication is enabled.
- Cookies are HttpOnly and SameSite-aware; secure-cookie behavior accounts for TLS and
  trusted proxy deployments.
- CSP nonces, frame protection, MIME sniffing protection, SRI generation, and restrictive
  permissions headers are present.
- The AI proxy bounds request and response sizes, validates model names, limits tool
  rounds, and applies endpoint rate limits.
- HTTP timeouts, WebSocket read limits, ping/pong deadlines, and global/per-IP connection
  caps are present.
- Collector response bodies are generally bounded, database pools are constrained, and
  queries use fixed SQL.
- The binary storage decoder has extensive bounds checks and fuzz coverage. Corrupt
  headers now fail closed instead of silently reinitializing data.
- Landlock and the hardened systemd unit provide useful defense in depth, while the code
  correctly treats unsupported Landlock enforcement as best-effort rather than a sole
  security boundary.
- CI grants read-only repository permissions, disables persisted checkout credentials,
  and pins GitHub Actions by commit SHA.
- Docker build stages use digest-pinned base images and the runtime container is
  unprivileged.

## Verification performed

The prescribed `./addons/check.sh` completed successfully:

- `govulncheck`: no reachable symbol or package vulnerabilities
- `gofmt`: clean
- `go vet ./...`: clean
- `go test -v -race ./...`: all packages passed
- `golangci-lint`: 0 issues

A separate verbose `govulncheck` run found no reachable vulnerabilities. It reported one
module-only advisory, `GO-2026-5932`, for the unmaintained
`golang.org/x/crypto/openpgp` package; Kula does not import or call that package, so it is
not a reachable project vulnerability.

The WebSocket lifecycle issue was validated with a temporary local regression test as
described in KUL-SEC-005. That test was removed after execution. No project source was
changed as part of this review.

## Review limitations at cutoff

- No live Kula instance or external host was scanned.
- The aggressive `kula-scan` checks were not run.
- Browser behavior was reviewed statically; no browser automation or DOM fuzzing was
  performed.
- Deployment-specific firewall, reverse proxy, filesystem ownership, and database TLS
  settings were not available.
- `./addons/build.sh` and the separate Semgrep workflow were not run in this cutoff;
  compilation was exercised throughout the race-enabled Go test suite.

## Recommended order of work

1. Make remote unauthenticated binding fail closed (KUL-SEC-001).
2. Move WebSocket deletion/channel closure wholly into the hub and add a churn race test
   (KUL-SEC-005).
3. Add verified TLS for database collectors (KUL-SEC-003).
4. Sign release metadata and repair the Ansible install path (KUL-SEC-002).
5. Harden storage ownership, no-follow opens, and atomic session persistence
   (KUL-SEC-004).
6. Bound history and custom-socket concurrency (KUL-SEC-006/007).
7. Add trusted-proxy CIDRs and per-redirect address validation (KUL-SEC-008/010).
8. Make configuration strict, reject unsupported tier counts, and address the remaining
   reliability items.
