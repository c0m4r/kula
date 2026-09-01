# Kula 0.19.0 — Security & Code Review

Full-surface audit of the collector, tiered storage engine, web server, authentication
layer, Landlock sandbox and browser dashboard.

| | |
|---|---|
| **Commit** | `6435f97` (branch `main`) |
| **Go source** | 19,721 LOC (non-test) |
| **Tests** | 10,595 LOC |
| **`go vet ./...`** | clean |
| **`go test ./... -race`** | all packages pass |
| **Findings** | 20 — **1 high, 4 medium, 15 low** |

Severity reflects **impact on a default deployment** — web UI enabled, authentication off,
which is what `config.example.yaml` ships.

Two findings are marked **VERIFIED**: I wrote a reproduction and watched it fail.
Everything else is read from source and reasoned about; where I could not confirm
exploitability I say so rather than inflating the rating.

---

## Contents

- [High](#high)
- [Medium](#medium)
- [Low](#low)
- [What holds up](#what-holds-up)
- [How this was verified](#how-this-was-verified)
- [Suggested order](#suggested-order)

---

## High

Remotely reachable, crashes the process, no authentication required in the default
configuration.

### K-01 · HIGH · VERIFIED — WebSocket disconnect races the metric broadcast and panics the process

**`internal/web/websocket.go:132` → `internal/web/server.go:1027`**

When a WebSocket client goes away, `unregister()` pushes the client onto `hub.unregCh` —
a *buffered* channel drained asynchronously by `hub.run()` — and then immediately closes
`client.sendCh`. It does not wait for the hub to actually remove the client from the map.

```go
unregister := func() {
    unregOnce.Do(func() {
        s.wsMu.Lock()
        // ...
        s.wsMu.Unlock()
        s.hub.unregCh <- client   // async: hub.run() drains this later
        close(client.sendCh)      // but the channel closes right now
    })
}
```

Meanwhile `broadcast()` holds `h.mu.RLock()` while iterating `h.clients`, which blocks
`hub.run()` from taking its write lock. So the client is still in the map when its channel
closes, and the next line executed is `client.sendCh <- data` on a closed channel. The
`select`/`default` does not save it: **a send to a closed channel panics rather than taking
the default branch.**

`BroadcastSample` is called from the main collection goroutine at `cmd/kula/main.go:216`
and `:227`, with no `recover()` anywhere in the path — so the panic takes down the entire
daemon, not just one connection.

The window is roughly one broadcast-iteration per one-second tick, so ordinary browser
reconnects will hit it eventually. An attacker who can open and close WebSocket
connections in a loop hits it in seconds, and `/ws` is unauthenticated whenever
`web.auth.enabled` is false.

**Reproduction**

```
$ go test ./internal/web/ -run TestHubBroadcastAfterUnregisterRace
panic: send on closed channel

goroutine 11 [running]:
kula/internal/web.(*wsHub).broadcast(0x0?, {0x16e8eac754d8, 0x1, 0x1})
    /home/kula/git/kula/internal/web/server.go:1027 +0x19b
```

The test does exactly what `handleWebSocket` does — push to `unregCh`, then close
`sendCh` — concurrently with a `broadcast()`.

**Recommendation**

Make the hub the sole owner of the channel's lifetime. Stop closing `sendCh` in
`unregister()`; instead close it inside `hub.run()`'s `unregCh` case, under the same
`h.mu.Lock()` that removes the client from the map, and only if the delete actually
removed it:

```go
case client := <-h.unregCh:
    h.mu.Lock()
    if _, ok := h.clients[client]; ok {
        delete(h.clients, client)
        close(client.sendCh)   // no broadcast can be mid-iteration here
    }
    h.mu.Unlock()
```

The write pump already handles `ok == false` correctly, so it needs no change.

A `defer recover()` in the write path would mask the symptom but leave the ownership bug,
and does not protect the collection goroutine — fix the ownership.

---

## Medium

Availability, session-lifetime and configuration-integrity issues. None grants access on
its own.

### K-02 · MEDIUM · VERIFIED — Container ID shorter than 12 bytes panics the collector goroutine

**`internal/collector/containers.go:284`**

```go
name := c.ID[:12]
```

This slices a value taken straight from the Docker/Podman API response with no length
check. An `Id` of `""` or anything under 12 characters is an immediate
`slice bounds out of range` panic. The container collector runs in its own goroutine
started by `Start()` with no `recover()`, so this kills the daemon.

Real Docker and Podman always return 64-character IDs, which is why this has never fired.
It matters because the socket is not always the daemon you think it is —
container-socket proxies (docker-socket-proxy, Podman's REST shim, a compromised sidecar)
sit in that position routinely, and the collector treats their output as trusted structure.

**Reproduction**

```
--- FAIL: TestShortContainerIDPanics
    panic reached: runtime error: slice bounds out of range [:12] with length 3
```

(Input: `[{"Id":"abc","Names":[],"State":"running"}]`)

**Recommendation**

```go
if c.ID == "" {
    continue
}
name := c.ID
if len(name) > 12 {
    name = name[:12]
}
```

While you are there, treat the rest of that response as untrusted too — see
[K-19](#k-19--low--container-id-interpolated-into-the-docker-api-url-path-unescaped).

---

### K-03 · MEDIUM — Login rate limiter allows targeted account lockout and global login denial

**`internal/web/server.go:850`, `internal/web/auth.go:119`**

```go
if !s.auth.UserLimiter.Allow(strings.ToLower(creds.Username)) {
```

The limiter is keyed on the **submitted username**, and five attempts in five minutes
closes that key. An unauthenticated attacker who knows or guesses the admin username can
spray five bad passwords every five minutes and keep the real operator locked out
indefinitely, from any address, for as long as they care to.

The `maxRateLimiterKeys` cap compounds it. `reserveRateLimiterKey` is deliberately
fail-closed: once 16,384 keys are tracked and a purge frees nothing, **every new key is
refused**. Since usernames are attacker-chosen and bounded only by the 4 KB body limit,
filling the username limiter takes 16,384 requests and then denies login to every user
whose name is not already tracked. The IP limiter has the same shape.

The memory-bound reasoning behind the cap is sound; the problem is that the two limiters
have different threat models and share one policy. **An IP key identifies the caller. A
username key identifies the *victim*.**

**Recommendation**

Key the second limiter on the pair — `username + client IP` — so an attacker throttles
only themselves, and reserve any username-only counter for a much higher threshold with an
exponential backoff rather than a hard block.

Separately, consider failing **open** on the username limiter when the map saturates (the
IP limiter still covers you) rather than converting memory pressure into a login outage.

---

### K-04 · MEDIUM — Sessions renew forever; no absolute lifetime

**`internal/web/auth.go:256`**

```go
// Sliding expiration
sess.expiresAt = time.Now().Add(a.cfg.SessionTimeout)
```

`ValidateSession` extends `expiresAt` by the full `session_timeout` on every successful
check. `createdAt` is recorded on the session, serialized to `sessions.json`, read back by
`LoadSessions` — and **never compared against anything**.

A token stays valid indefinitely as long as it is used once per timeout window, which the
dashboard's WebSocket and polling do automatically as long as a tab is open. With the
default 24-hour timeout that means a token captured from a browser profile, a proxy log or
a backup of the storage directory works for as long as the attacker keeps a tab open.
Sessions also survive restarts by design, so there is no natural reset point.

The absence of session rotation on login compounds this slightly: `CreateSession` always
mints a new token, but nothing invalidates the previous one, so sessions accumulate per
login until they expire.

**Recommendation**

Enforce the field you already store. In `ValidateSession`, reject and delete when
`time.Since(sess.createdAt) > absoluteTimeout` **before** applying the sliding extension.
A separate `web.auth.session_max_lifetime` (default something like 7 days) keeps it
configurable; a reasonable interim is to cap at a fixed multiple of `session_timeout`.

---

### K-05 · MEDIUM — Changing `storage.tiers[].max_size` silently does nothing to existing tiers

**`internal/storage/tier.go:135`**

`OpenTier` computes `maxData` from the configured `maxSize` and validates it
(`maxData < 1024` → error), then `readHeader` overwrites `t.maxData` with the value
persisted in the file header:

```go
t.maxData = int64(binary.LittleEndian.Uint64(buf[16:24]))
if t.maxData == 0 {
    return fmt.Errorf("invalid header: maxData is zero")
}
```

The only check applied to the on-disk value is `!= 0`. Nothing reconciles the two, and
nothing warns.

An operator who raises `max_size` to keep more history gets no more history and no error.
One who lowers it to reclaim disk gets no reclaim. The ring keeps running at whatever size
it was first created with, forever, and the only way to apply the config is to delete the
tier file — which throws away the data they were trying to keep. **This is a data-retention
promise the configuration appears to make and does not honour.**

The header value winning is defensible on its own — it is what the existing ring geometry
actually is, and reading with the wrong `maxData` would corrupt the wrap arithmetic. The
bug is that the mismatch is invisible.

**Recommendation**

At minimum, compare the header's `maxData` against the configured value in `OpenTier` and
log a clear warning naming both, so the setting does not appear to have been applied.
Better: grow in place when the config is larger (the ring can be extended past `writeOff`
safely when not wrapped) and document that shrinking requires a rebuild.

---

## Low

Hardening, robustness and consistency. Each is individually minor; several are one-line
fixes worth taking while the file is open.

### K-06 · LOW — `kula -config <new path> -version` writes a config file before printing the version

**`cmd/kula/main.go:73`**

The seed-from-example block runs before the `showVersion` check, so a read-only command
creates a file on disk as a side effect. Harmless in practice, surprising in a packaging
script or a healthcheck.

**Fix:** move the `showVersion || showVersionShort` early-exit above the config-seeding
block.

---

### K-07 · LOW — `nvidia.log` is read unbounded into memory once per collection tick

**`internal/collector/gpu_nvidia.go:44`**

`io.ReadAll(f)` has no size cap, and the file is written by an operator-supplied exporter
script. An exporter that appends instead of truncating grows the file without limit, and
Kula re-reads and re-splits the whole thing every second.

Every other external input in the collector is capped — nginx at 4 KB, apache2 at 64 KB,
the container socket at 1 MB — this one is not.

**Fix:** wrap in `io.LimitReader` with a cap sized for the largest plausible multi-GPU CSV
(64 KB is generous), and log once when the cap is hit.

---

### K-08 · LOW — Upstream error text is written into an SSE frame without escaping newlines

**`internal/web/ollama.go:361`**

```go
_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
```

The error can carry up to 512 bytes of the upstream response body verbatim
(`doStreamRound`'s non-200 path). A body containing `\n\n` terminates the frame early and
lets the rest be parsed as additional SSE events by the browser client.

Reaching it requires control of the configured Ollama endpoint, which config validation
pins to loopback — so this is defence in depth, not a live path.

**Fix:** `strings.ReplaceAll(err.Error(), "\n", " ")` before formatting, matching how the
content path already escapes newlines.

---

### K-09 · LOW — Model name interpolated into `innerHTML` without escaping

**`internal/web/static/js/app/ollama.js:49`**

```js
select.innerHTML = `<option value="${ollamaModel}">${ollamaModel}</option>`;
```

The value comes from `/api/config`'s `ollama_model`, i.e. the operator's own YAML, so it is
not attacker-reachable. It is the single unescaped interpolation in a frontend that is
otherwise careful — every other dynamic option is built with `createElement` +
`textContent`, including `updateModelSelector` forty lines below, which handles the same
data correctly.

**Fix:** build the seed option the same way `updateModelSelector` does.

---

### K-10 · LOW — Postgres DSN escapes the password but not the fields beside it

**`internal/collector/postgres.go:50`**

The password gets careful backslash and quote escaping; `host`, `user`, `dbname` and
`sslmode` are interpolated raw into a space-separated `key=value` DSN. A `dbname`
containing a space silently becomes an extra connection parameter — e.g.
`dbname: "app sslmode=disable"` quietly downgrades TLS.

Config-controlled, so not an injection vector, but the asymmetry is the kind that surprises
later.

**Fix:** apply the same quoting helper to all four fields.

---

### K-11 · LOW — `postgres.sslmode` defaults to `disable`

**`internal/config/config.go:451`, `config.example.yaml:334`**

Sensible for the loopback case the feature was designed around, but `postgres.host` accepts
any address. An operator pointing it at a remote database gets cleartext credentials and
query traffic with no warning. The Ollama URL gets a hard loopback check; this gets nothing.

**Fix:** keep the default but log a warning at startup when `sslmode` is `disable` and the
host is neither loopback nor a Unix socket path.

---

### K-12 · LOW — nginx and apache2 status URLs get no scheme or host validation

**`internal/collector/nginx.go:30`, `internal/collector/apache2.go:33`**

`validateOllamaURL` pins the Ollama endpoint to loopback specifically to stop a malicious
config file turning Kula into an SSRF pivot. The two status URLs go through no equivalent
check — they are parsed only in `sandbox.Enforce`, and only to extract a port number for
the Landlock `ConnectTCP` rule, which is port-scoped rather than address-scoped.

The trust boundary is the config file either way, so this is consistency rather than a new
exposure. Worth closing because the Ollama check establishes the expectation that
config-supplied URLs are constrained.

**Fix:** validate both at load time — require `http`/`https`, reject credentials in the
URL, and warn on a non-loopback host.

---

### K-13 · LOW — Custom-metrics socket is removed blind, unlike the web socket

**`internal/collector/custom.go:107`**

```go
_ = os.Remove(sockPath)
```

No checks at all. The web listener does this properly in `removeStaleUnixSocket`: it
refuses to unlink a path that is not a socket, and refuses to steal one another process is
actively listening on. The custom-metrics path has neither guard, so a second Kula instance
sharing a storage directory silently hijacks the first instance's socket and its producers
reconnect to the wrong daemon.

Contained by the path being fixed at `<storage_dir>/kula.sock` rather than
operator-supplied.

**Fix:** reuse `removeStaleUnixSocket`'s logic — it is already written and tested.

---

### K-14 · LOW — Custom-metrics socket accepts unbounded concurrent connections

**`internal/collector/custom.go:162`**

`acceptLoop` spawns a goroutine per connection with no cap, and each allocates a 64 KB
scanner buffer held for up to the 5-minute idle timeout. The socket is mode 0660, so any
member of the owning group can open connections in a loop and consume file descriptors and
memory. Local and group-scoped, which is why this is low.

**Fix:** a counting semaphore in `acceptLoop` — a few dozen concurrent producers is far
above any real use.

---

### K-15 · LOW — Sandbox doc comment says read-only on the container socket; the code grants read-write

**`internal/sandbox/sandbox.go:42` vs `:166`**

The `Enforce` doc comment promises "`ROFiles` on the Docker/Podman Unix socket"; the
implementation uses `landlock.RWFiles(socketPath)`. Write access is genuinely required —
the Docker API is a request/response protocol over that socket — so the code is right and
the comment is wrong.

It matters more than a typo because **write access to the Docker socket is effectively root
on the host**, and this is the one comment a reader consults to understand what the sandbox
actually permits.

**Fix:** correct the comment and state plainly why RW is required and what it implies.

---

### K-16 · LOW — utmp parsing uses `f.Read` where it means `io.ReadFull`

**`internal/collector/system.go:95`**

```go
n, err := f.Read(buf)
if n < recordSize || err != nil {
    break
}
```

This treats a short read as end-of-file. `io.Reader` is permitted to return fewer bytes
than requested without an error, in which case the remaining records are silently dropped
and the logged-in-user count reads low. Regular files on Linux essentially always return
the full request, so this will not fire in practice.

**Fix:** `io.ReadFull(f, buf)` and break only on `io.EOF` / `io.ErrUnexpectedEOF`.

---

### K-17 · LOW — CSP carries `style-src 'unsafe-inline'` that nothing appears to need

**`internal/web/server.go:241`**

Neither `index.html` nor `game.html` contains a `<style>` block or a single `style=`
attribute — I checked both (`grep -c 'style="'` → 0 for each). The frontend styles elements
through `element.style.*`, which is CSSOM and is **not** governed by `style-src` at all.
The `script-src` directive is already properly nonce-based, so this is the one weak link in
an otherwise tight policy.

**Fix:** drop `'unsafe-inline'` and load the dashboard — verify Chart.js in particular,
since some builds inject inline styles for tooltips and the responsive canvas. If it does
need it, keep it and add a comment recording why, so the next reader does not re-open this.

---

### K-18 · LOW — CSP nonce ignores the `crypto/rand` error

**`internal/web/server.go:229`**

```go
_, _ = rand.Read(b)
```

On a hypothetical failure the buffer stays zeroed and every response ships the same
predictable nonce, which defeats the nonce-based `script-src`. Modern Go makes
`crypto/rand.Read` effectively infallible (it panics internally rather than returning an
error), so this is theoretical.

**Fix:** return 500 on error rather than serving a page with a degraded CSP.

---

### K-19 · LOW — Container ID interpolated into the Docker API URL path unescaped

**`internal/collector/containers.go:384`**

```go
resp, err := cc.client.Get(fmt.Sprintf("http://localhost/containers/%s/json", id))
```

`id` comes from the daemon's own earlier response. An ID containing path separators or `..`
would address a different Docker API endpoint than intended. Same trust question as
[K-02](#k-02--medium--verified--container-id-shorter-than-12-bytes-panics-the-collector-goroutine):
fine against a real daemon, not fine against a socket proxy.

**Fix:** `url.PathEscape(id)`, and reject IDs that are not plain hex before using them.

---

### K-20 · LOW — Chat history and context are injected into the system prompt unfiltered

**`internal/web/ollama.go:290`, `:836`**

`sanitisePrompt` strips nulls and clamps `req.Prompt` to 2,000 runes — but `req.Context` is
written verbatim into the **system** message, and `req.Messages` is appended with whatever
`Role` the client chose, including `system` and `tool`. Both are bounded only by the 32 KB
body cap.

Impact is limited because the caller is the authenticated user, who can say anything in a
normal turn anyway, and the single exposed tool (`get_metrics`) is read-only and
storage-bounded. Worth noting because the code goes to real trouble validating the model
name against a regex in the same handler, so the inconsistent rigour looks unintentional.

**Fix:** clamp `req.Context` to a documented length and reject any client-supplied message
whose role is not `user` or `assistant`.

---

## What holds up

Reviews that only list defects misrepresent the codebase. These are the areas I actively
tried to break and could not.

**Password handling** — Argon2id with configurable parameters, per-user salts,
`subtle.ConstantTimeCompare` throughout, and a dummy-hash fallback that closes the
username-enumeration timing oracle, with the reasoning documented in the code.

**Session storage** — Tokens are SHA-256 hashed before they touch the map or
`sessions.json`, written 0600. A stolen sessions file yields no usable tokens.

**No SQL injection** — Every Postgres and MySQL statement is a compile-time constant. No
user input reaches a query, in any collector.

**Binary codec bounds** — `decodeVariable`'s `need()` helper checks remaining length before
every field read, all counts are `uint16`-bounded, and `getStr` validates its own length
prefix. Backed by fuzz targets.

**Frontend escaping** — `escapeHTML` covers all five characters, `renderMarkdownLite`
escapes *before* it transforms (and its only attribute interpolation is `\w*`-constrained),
and dynamic options are built with `createElement`. One exception, K-09.

**Corrupt-tier handling** — An unparseable header is refused rather than reinitialised in
place, with an error that tells the operator to move the file aside. Someone clearly
learned that lesson the hard way.

**Landlock sandbox** — V5 with `BestEffort` degradation, rules narrowed to the paths and
ports actually configured. Uncommon to see in a tool this size.

**Security defaults** — Headers, frame protection and origin validation all default to on.
Cookies are `HttpOnly` with `SameSite=Strict` and conditional `Secure`. Startup warnings
fire on risky combinations.

**Installer supply chain** — Both installers verify SHA-256 checksums against the release
manifest, and skipping verification is an explicit opt-in.

**Test posture** — 10,595 lines of tests including runtime security tests driving the real
middleware stack, fuzz targets on the codec, config and `/proc` parsers, plus `kula-scan`
as an over-the-wire black-box checker.

---

## How this was verified

| Check | Command | Result |
|---|---|---|
| Build | `go build ./...` | clean |
| Vet | `go vet ./...` | clean |
| Full suite, race detector | `go test ./... -race -count=1` | all packages pass |
| K-01 reproduction | `go test -run TestHubBroadcastAfterUnregisterRace` | panic reproduced |
| K-02 reproduction | `go test -run TestShortContainerIDPanics` | panic reproduced |
| MySQL DSN special characters | `mysql.ParseDSN`, 5 payloads | **no defect — hypothesis withdrawn** |

The last row is included deliberately. I suspected the unescaped MySQL password
interpolation at `internal/collector/mysql.go:41` could misparse into the wrong DSN field,
tested it against the pinned driver (`v1.10.0`) with passwords containing `@`, `/`, `?` and
a full `x@tcp(evil:3306)/y` injection payload, and every one round-tripped correctly. It is
not a finding and is not listed as one.

Nothing in the working tree was modified — reproductions were written to a scratch
directory and removed.

---

## Suggested order

Ordered by risk retired per unit of work, not by severity alone.

1. **K-01** — the only finding an unauthenticated remote party can use to take the daemon
   down. Contained fix in `hub.run()`; add a regression test that runs the disconnect and
   broadcast concurrently.
2. **K-02** — two lines, removes a second crash path.
3. **K-03, K-04** — both touch `auth.go` and both change observable authentication
   behaviour, so they are worth landing together with tests.
4. **K-05** — the warning alone is small and stops the setting quietly lying; in-place
   growth can follow separately.
5. **K-06 … K-20** — mostly one-liners. Batching them by file (`ollama.go`,
   `containers.go`, `custom.go`) keeps the diff reviewable.

---

*Reviewed against commit `6435f97` on the `main` branch — 19,721 lines of non-test Go
across the collector, storage, web, sandbox, backup, TUI and i18n packages, plus the
embedded dashboard JavaScript, packaging scripts and configuration defaults. Severity
ratings assume the default shipped configuration with authentication disabled; findings
K-01 and K-02 were confirmed by reproduction, the rest by source analysis.*
