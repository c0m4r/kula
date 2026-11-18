# kula-scan

**Black-box safeguard scanner for a running Kula instance.**

The unit tests, fuzz targets and in-process runtime tests verify Kula's defenses in the lab.
`kula-scan` verifies them in the field: it points at a live Kula URL, probes it over HTTP,
raw sockets, TLS and WebSocket the way a browser or an attacker would, and reports per finding
whether each safeguard holds in that deployment: behind your proxy, with your TLS, your base
path and your config. It imports nothing from Kula's `internal/` packages; every judgement is
made from what the target answers.

This page is the reference for operators. How the scanner is built and extended is in
[kula-scan (developer notes)](../../docs/dev/13-kula-scan.md).

- [Get it](#get-it) · [Quick start](#quick-start) · [Flags](#flags)
- [Statuses](#statuses) · [Exit status](#exit-status) · [Declaring the posture: `-expect`](#declaring-the-posture--expect)
- [Selecting and accepting findings](#selecting-and-accepting-findings) · [Reaching the target](#reaching-the-target) · [Reports](#reports)
- [What it checks](#what-it-checks) · [Fuzzing](#fuzzing--fuzz) · [What a run costs the target](#what-a-run-costs-the-target) · [What it cannot tell](#what-it-cannot-tell)

## Get it

Each release ships the scanner per architecture as `kula-scan-linux-<version>-<arch>.gz`
(`amd64`, `arm64`, `riscv64`), listed in the release's `CHECKSUMS.sha256.txt`. The packages,
tarballs and Docker image leave it out. Download and verification:
[Troubleshooting → Diagnostics checklist](../../docs/user/16-troubleshooting.md#diagnostics-checklist).
Take the release that matches the target's `kula --version`: a newer scanner judges an older
Kula against safeguards it does not have yet, and says so in the remediation.

From a checkout (it is part of the same Go module, so `go vet ./...` and `go test ./...` cover
it):

```bash
go build -o kula-scan ./cmd/kula-scan
./addons/build.sh            # builds kula, kula-scan and gen-mock-data
```

## Quick start

```bash
# Default scan: read-mostly, no credentials
kula-scan http://localhost:27960

# Authenticated, over TLS; the password from the environment, not the command line
read -rs KULA_SCAN_PASSWORD && export KULA_SCAN_PASSWORD
kula-scan -username admin https://mon.example.com

# A CI gate: the posture this deployment must have, a SARIF artifact, the text report in the log
kula-scan -expect auth,tls,hsts -username admin -format sarif -o kula-scan.sarif https://mon.example.com

# Disruptive probes and fault injection (staging, or accept the side effects below)
kula-scan -aggressive -fuzz -username admin http://localhost:27960
```

`kula-scan -list` prints every finding ID with its category, mode and title; `kula-scan -h`
prints the flags, examples and exit statuses.

## Flags

`kula-scan [flags] <target-url>`. The target is an `http://` or `https://` URL, optionally with
the base path Kula is mounted under (`http://10.0.0.5:27960/kula`).

**Target and connection**

| Flag | Meaning |
|------|---------|
| `-target URL` | The target, instead of the positional argument. |
| `-base-path PATH` | Base path Kula is mounted under (`web.base_path`), e.g. `/kula`. Overrides a path in the URL. |
| `-connect host:port` | Connect here instead of the URL's address; the URL still gives the `Host` header, the TLS server name and certificate name, the `Origin` values and the base path. |
| `-unix PATH` | Connect over this Unix socket (`web.unix_socket`); the URL still gives the `Host` header and base path. |
| `-H 'Name: value'` | A header sent on every request, for a gateway in front of Kula. Repeatable; values are never printed. See [Reaching the target](#reaching-the-target). |
| `-timeout D` | Per-request timeout (default `10s`). |
| `-insecure` | Skip TLS certificate verification in the handshake; `TLS-004` then verifies the chain itself. |

**Credentials**

| Flag | Meaning |
|------|---------|
| `-username NAME` | Account for the authenticated checks. It needs a password: without one, from `-password` or `KULA_SCAN_PASSWORD`, the scan does not start (exit 2), so a CI secret that is not set does not turn into a scan without a session. |
| `-password PW` | Its password, only with `-username` (alone it is a usage error). Without it, the password comes from `KULA_SCAN_PASSWORD`, which keeps it out of the process list and the shell history; that variable set without `-username` is no error, and the scan runs without a session. |
| `-metrics-token T` | The bearer token `/metrics` should accept (`PROM-003`); or `KULA_SCAN_METRICS_TOKEN`. Never printed. |

**What runs**

| Flag | Meaning |
|------|---------|
| `-only LIST` | Categories and/or finding IDs to run, comma-separated, case-insensitive. |
| `-skip LIST` | Categories and/or finding IDs not to run or report. |
| `-aggressive` | Enable the disruptive checks (see [costs](#what-a-run-costs-the-target)). |
| `-dos-wait D` | How long the slow-connection probes wait for Kula to drop a stalled connection (default `35s`, for Kula's 30 s `ReadTimeout`). |
| `-fuzz` | Enable blind fault injection. |
| `-fuzz-iter N` | Requests per randomized fuzz probe (default 200). |
| `-seed N` | PRNG seed for fuzzing; 0 picks one and reports it. |

**Gate**

| Flag | Meaning |
|------|---------|
| `-fail-on SEV` | Lowest FAIL severity that exits 1: `info`, `low`, `medium`, `high` (default), `critical`. |
| `-expect LIST` | Posture the target must have; see [`-expect`](#declaring-the-posture--expect). |
| `-ignore LIST` | Finding IDs that stay in the report, marked ignored, but never affect the exit status. |

**Output**

| Flag | Meaning |
|------|---------|
| `-format F` | Report format: `text` (default), `json`, `junit`, `sarif`. |
| `-json` | The same as `-format json`; conflicts with any other `-format`. |
| `-o FILE` | Write the `-format` report to `FILE`; stdout then gets the text report. |
| `-no-color` | No ANSI colors. Colors are also off when stdout is not a terminal or `NO_COLOR` is set. |
| `-v` | Log each request and its response status to stderr. |
| `-list` | Print the catalog of finding IDs and exit. |
| `-version` | Print the version and exit. |

The report goes to stdout and nothing else does. Progress lines (`[7/32] traversal: TRAV-000,
…`), the `-v` request log, the `-aggressive` and `-fuzz` warnings and errors go to stderr;
progress lines only when stderr is a terminal or with `-v`.

An unknown flag, category, finding ID, `-expect` key, `-format` or `-fail-on` value, a malformed
`-H`, `-connect` or base path, `-connect` with `-unix`, `-username` without a password,
`-password` without `-username`, and an `-expect` key the run cannot judge
([below](#declaring-the-posture--expect)) are usage errors (exit 2) before anything is sent.

## Statuses

| Status | Meaning |
|--------|---------|
| `PASS` | The safeguard is present and the target's answer shows it. |
| `FAIL` | The safeguard is missing or bypassable. Only a FAIL at or above `-fail-on` (or one that contradicts `-expect`) changes the exit status. |
| `WARN` | An operator choice or a weak posture: auth off, origin validation off, `/metrics` without a token, an HSTS max-age under 180 days. `-expect` turns the ones it names into FAILs. |
| `SKIP` | Not applicable, with the reason: the feature is off, no UI or API at the address, no credentials, an aggressive or fuzz check without its flag, a limiter already spent, a store too young to show the history cap. |
| `ERROR` | The probe could not judge: some of its requests failed or went unanswered, the credentials were refused, `/api/auth/status` could not be read, the answers say nothing of the safeguard. An ERROR is never a pass; it makes the run inconclusive (exit 3). |

A check passes only when every request it sent was answered. One that found nothing wrong in
the answers it got, while some requests got none, is an ERROR that names them; a FAIL or WARN
found in the answers stands, and names them too. Over a raw socket, a close or reset without a
response refuses the request, but a request the target neither answers nor closes within
`-timeout` got no answer: the traversal probes (`TRAV-000`–`TRAV-002`), `HOST-001` and
`FUZZ-SMUGGLE` are then an ERROR, not a pass.

A FAIL or WARN carries a severity (`INFO` to `CRITICAL`), shown in brackets in the text report,
and usually a `fix:` line. An ID that does not apply may be left out rather than skipped, for
example: `HDR-000` and `TRAV-000` (the controls the other checks rely on) appear only when the
control fails, `HDR-009` only when a `Server` header is sent, `HDR-004` only when the CSP carries
a nonce, `CORS-002` only when CORS headers are sent, `PROM-002` only when `/metrics` asks for a
token; `AUTH-001`–`AUTH-005`, `AUTH-010`, `CSRF-003`, `CSRF-004` and `WS-001` only with auth
on, and `AUTH-007`–`AUTH-009` only after a login (`AUTH-009` only over HTTPS).

## Exit status

| Code | Meaning |
|------|---------|
| `0` | Nothing fails at or above `-fail-on`, every `-expect` holds and every probe completed. |
| `1` | A FAIL at or above `-fail-on`, or a FAIL against a declared `-expect`, whatever `-fail-on` says. |
| `2` | Usage error; the target is unreachable or not Kula (`DISC-001` is an ERROR); or `-o` or the report could not be written. |
| `3` | Inconclusive: nothing gates, but a probe ended in ERROR, e.g. refused credentials, a refused `Host` name (`DISC-003`), an `/api/auth/status` that could not be read (`AUTH-000`) or an `-expect` key that could not be judged. |
| `130` | Interrupted by SIGINT or SIGTERM: the scan logs out and prints the partial report. A login in flight is seen through, within `-timeout`, so its session is logged out too; no login starts after the signal. A second signal quits at once. |

They are checked in the order 130, 2, 1, 3: a FAIL wins over an ERROR. Ignored findings count for
nothing, except that a target that could not be scanned at all still exits 2. Any non-zero exit
ends with one line on stderr that says why, e.g.

```
kula-scan: exit 3: inconclusive: no failing safeguard, but these probes could not complete: AUTH-005
```

A FAIL below `-fail-on` is reported but exits 0: at the default `high`, a missing `nosniff`
(`HDR-001`, medium) does not gate. Use `-fail-on medium` or lower for a stricter gate.

## Declaring the posture: `-expect`

Kula lets an operator turn safeguards off, and the scanner reports that as a WARN, which never
gates. `-expect` declares what this deployment must have: a WARN tagged with a declared key
becomes a FAIL that exits 1 whatever `-fail-on` is, and the text report marks it
`(expected: key)`.

| Key | The target must… | Findings |
|-----|------------------|----------|
| `auth` | have `web.auth` enabled | `AUTH-000` |
| `tls` | be served over HTTPS with a certificate clients trust | `DISC-002`, `TLS-001`, `TLS-004` |
| `hsts` | be served over HTTPS with `Strict-Transport-Security` | `HDR-008` |
| `origin-validation` | refuse a state change without an `Origin` or from a foreign one, and a cross-origin WebSocket upgrade | `CSRF-001`, `CSRF-002`, `WS-002` |
| `frame-protection` | forbid framing (`web.security.frame_protection`) | `HDR-002`, `HDR-007` |
| `metrics-token` | require a bearer token on `/metrics` | `PROM-001` |
| `host-check` | refuse `Host` names a public DNS server could answer for | `HOST-001` |

Each key is judged by the findings in its row and by no other. Over plain HTTP, `-expect tls`
fails `TLS-001` and `-expect hsts` fails `HDR-008`, which are otherwise SKIPs. `-expect hsts`
fails a missing, invalid or zero `max-age`, and one under 180 days.

A key the run cannot judge would pass unexamined, so it is refused:

- At startup, a declared key whose findings are all deselected (`-only`, `-skip`), all ignored
  (`-ignore`), or all of a mode the run does not enable is a usage error (exit 2) that names
  the key, its findings and why each is out: `-expect host-check -skip HOST-001` does not
  start. One of its findings left in is enough.
- During the run, a declared key's finding that was skipped because the auth posture is
  unknown ([below](#when-kula-serves-no-ui-or-api-or-its-auth-posture-cannot-be-read)) is an
  ERROR (exit 3), never a FAIL or a SKIP.

## Selecting and accepting findings

- `-only` and `-skip` take categories (`discovery`, `headers`, `auth`, `csrf`, `cors`, `host`,
  `traversal`, `metrics`, `ws`, `input`, `redirect`, `integrity`, `tls`, `rate`, `dos`,
  `bypass`, `fuzz`) and finding IDs, mixed. A check runs when any ID it judges is selected and
  reports only the selected ones, so `-only CSRF-003` still sends the other CSRF probes.
  `DISC-001`, which says whether the target was scanned at all, is always reported. So are
  `DISC-003` and `AUTH-000` when a refused `Host` name or an unreadable `/api/auth/status` kept
  a selected check from running, unless `-skip` names them or their category.
- `-ignore` takes finding IDs. An ignored finding keeps its status, is marked `(ignored)` and
  counted apart in the summary, and never affects the exit status: use it for an accepted risk
  you still want to see.
- `-list` prints the catalog, the authoritative list of IDs.

```bash
kula-scan -only headers,traversal,cors http://10.0.0.5:27960
kula-scan -skip HDR-009 -ignore PROM-001 http://10.0.0.5:27960
```

## Reaching the target

- **Base path.** A path in the URL, or `-base-path`, is prefixed to every request, raw and
  WebSocket ones included. It is normalised to one leading slash and no trailing one; a query,
  fragment, whitespace or a whole URL is a usage error.
- **`-connect host:port`** scans by address under the real name, or a backend behind its proxy:
  `kula-scan -connect 10.0.0.5:443 https://mon.example.com`. IPv6 literals take brackets, in the
  URL and in `-connect`: `http://[::1]:27960`, `-connect [::1]:27960`.
- **`-unix path`** scans Kula's Unix socket: `kula-scan -unix /run/kula/kula.sock http://localhost/`.
  `HOST-001` is a SKIP there: Kula checks no `Host` name on a socket.
- **`-H 'Name: value'`** adds a header to every request (HTTP client, raw sockets and WebSocket
  handshakes), for an access proxy's token or basic auth at a proxy. A probe's own header of the
  same name wins, except `Cookie`, which carries both, the `-H` cookies first. Refused, because
  the scanner frames requests itself or the probes send them on purpose: `Host` (use
  `-connect`), `Content-Length`, `Transfer-Encoding`, `Connection`, `Upgrade`,
  `Sec-WebSocket-*`, `Origin`, `Referer`, `X-CSRF-Token`, and a `Cookie` that carries
  `kula_session` (the scanner's session comes from `-username`). Values are never printed: not in
  the `-v` log, a finding or a report; the JSON report lists the names.

### When Kula refuses the scanner's `Host` name

With auth off, Kula answers only local `Host` names (addresses, `localhost`, single-label names,
local-only domains, the machine's names, and the names in `web.security.allowed_hosts` and
`allowed_origins`): its DNS-rebinding check. Scanning such an instance by a public name gets
that check's 403 on every route but `/health`, `/status` and a `/metrics` that asks for a bearer
token. Discovery recognises it and reports `DISC-003` as an ERROR (exit 3).
Only the checks that still judge something run (headers, auth, TLS, `HOST-001`, `DOS-SLOWLORIS`,
`DOS-CONNFLOOD`); the rest are SKIPs that say why, since they would read the refusal as their own
safeguard holding. Rescan by address, or with `-connect <ip:port>` and a URL naming a host Kula
allows, or add the name to `web.security.allowed_hosts`.

### When Kula serves no UI or API, or its auth posture cannot be read

Discovery learns the auth posture from `/api/auth/status`. When it cannot, the posture is
unknown, never taken for auth off, and `DISC-001`'s detail says which of two cases applies:

- **No UI or API.** `/api/auth/status` answers a plain 404, not Kula's JSON: `web.ui: false`,
  which serves only `/health`, `/status` and `/metrics`, or a proxy that forwards only those.
  The checks that need the UI or API are SKIPs that say why, `AUTH-000` included, and the run
  can exit 0.
- **Unreadable.** The request fails or times out, or the answer is a 5xx or anything else but
  Kula's JSON. The same checks are SKIPs, and `AUTH-000` is an ERROR (exit 3) that says to check
  the URL and that a proxy in front forwards `/api/`. It is reported even when not selected,
  whenever the unknown posture kept a selected check from running, unless `-skip` names it.

The checks that still run need neither the UI nor the API, nor the auth posture: the `tls` and
`metrics` categories, `REDIR-001`, `DOS-SLOWLORIS`, `DOS-HEADERBOMB`, `DOS-CONNFLOOD`,
`FUZZ-PATH` and `FUZZ-LIVENESS`. A finding of a declared `-expect` key that is skipped this way,
`AUTH-000` under `-expect auth` among them, is an ERROR, never a FAIL. The JSON report's
`target_info.auth_enabled` is `null` while the posture is unknown.

## Reports

**text** (default) groups the findings by category in run order, with detail, `evidence:` and
`fix:` lines, a summary line, and the IDs that gate the exit status. Anything quoted from the
target (header values, status lines, certificate names) has its control characters, bidi
controls and invalid UTF-8 shown as escapes (`\x1b`, `\u202e`), so a hostile target cannot
rewrite earlier lines or set your clipboard.

**json** carries the run's metadata and the findings:

| Field | Content |
|-------|---------|
| `scanner` | `name`, `version` |
| `target` | The URL scanned, with its base path |
| `started`, `duration_ms` | UTC start (RFC 3339, whole seconds) and duration |
| `options` | `aggressive`, `fuzz`, `seed` (with `-fuzz`), `only`, `skip`, `ignore`, `expect`, `fail_on`, and `connect`, `unix`, `headers` (names only) when given |
| `target_info` | What discovery learnt: `https`, `auth_enabled` (`null` while the auth posture is unknown), `ollama`, `version` (when `/api/config` discloses it). Behind auth, `ollama` and `version` are what the run's session read of `/api/config`, and stay unset when the run opened no session |
| `findings[]` | `id`, `category`, `title`, `severity`, `status`, `detail`, `evidence`, `remediation`, `expectation` (the declared key it was judged against), `ignored` |
| `summary` | Counts: `pass`, `fail`, `warn`, `skip`, `error`, `ignored` (an ignored finding counts only there) |
| `interrupted`, `not_run`, `exit_code` | Whether a signal cut the run short, how many checks did not run, and the exit status |

Strings escape the same control characters as `\u` escapes, so the JSON decodes to the text the
target sent and is safe to print.

**junit** has one `<testsuite>` per category and one `<testcase>` per finding (`ID Title`,
classname `kula-scan.<category>`), and fails exactly when the exit status is not 0: a FAIL that
gates is a `<failure>` (type = severity), an ERROR an `<error>`, a SKIP or an ignored finding
`<skipped>`; a PASS, a WARN and a FAIL below `-fail-on` pass. An exit status no finding explains
(an interrupt, or a `DISC-001` ERROR that `-ignore` names) gets a run-level `Scan completed`
test case with an `<error>`. The first suite's `<properties>` carry the JSON metadata flattened
(`options.fail_on`, `target_info.https`, `exit_code`, …), a JSON `null` as `null`; each test
case's `<system-out>` keeps the whole finding.

**sarif** (2.1.0) has one run whose rules are the whole catalog (ID, title, description,
category and mode). FAIL and WARN findings are results: level `error` for a FAIL that gates,
`warning` for a FAIL below `-fail-on`, `note` for a WARN; an ignored one keeps its level and
carries an `external` suppression. Every result's location is the target URL. PASS and SKIP are
not results. An ERROR is a tool execution notification on the invocation, whose
`executionSuccessful` is false for exit statuses 2, 3 and 130 and whose `exitCodeDescription`
is the closing line; the invocation's properties carry `options`, `target_info`, `interrupted`
and `not_run` as the JSON report does. Each result carries `severity`, `status`, `expectation`
and a numeric `security-severity` (critical 9.5, high 8.0, medium 5.5, low 3.0) in its
properties. GitHub code scanning reads `security-severity` from the rule, so each rule with
results carries the highest among them, which GitHub shows as each alert's severity; a rule
without results carries none.

**`-o FILE`** writes the `-format` report to the file and the text report to stdout, so a CI job
keeps a readable log and an artifact from one run. The file is opened before the scan starts (an
unwritable path fails at once, exit 2) and created with mode 0600, since the report maps the
target's weak spots; an existing file keeps its mode.

## What it checks

`-list` is the authoritative list. Each category below says what is sent and which answer passes;
"Kula code" is where the safeguard lives (see the [Security Model](../../docs/dev/08-security.md)).
Mode: **default** runs on every scan; **aggressive** needs `-aggressive`; **fuzz** needs `-fuzz`.

### discovery

`GET /health` (`kula is healthy`), `/api/auth/status` (`auth_required`) and `/api/config`
(`ollama_enabled`, `version`; open only with auth off). Behind auth, the run's session reads
`/api/config` once instead, which tells the AI probes whether the assistant is on. A target that
does not answer, or is not Kula, is a `DISC-001` ERROR and the scan stops (exit 2; the detail
suggests the base path). An `/api/auth/status` that carries no `auth_required` and is not the
`Host` check's refusal leaves the auth posture unknown
([above](#when-kula-serves-no-ui-or-api-or-its-auth-posture-cannot-be-read)), and `DISC-001`'s
detail says why.
`DISC-002` warns when auth is on over plain HTTP (`-expect tls`). `DISC-003` is the refused
`Host` name above.

### Checks by category

| Category | Mode | What is sent, and what passes | Kula code |
|----------|------|-------------------------------|-----------|
| `headers` | default | `GET /`, `/style.css` and `/api/auth/status`. `nosniff` on all three (`HDR-001`); `X-Frame-Options: DENY` (`HDR-002`); the CSP judged by directive (below); a different nonce on a second `GET /` (`HDR-004`); `Referrer-Policy`, `Permissions-Policy`; over HTTPS, HSTS with a `max-age` of at least 180 days (`HDR-008`); no `Server` banner (`HDR-009`); `Cache-Control: no-store` on every 2xx of `/`, `/api/auth/status` and, when reachable, `/api/config` (`HDR-010`). | `securityMiddleware`, `contentSecurityPolicy`, `noStore` (`internal/web/server.go`) |
| `auth` | default | With auth off, only `AUTH-000` (a WARN, `-expect auth`). With auth on: the protected routes without a session answer 401 (`AUTH-001`); five forged session cookies and a bogus bearer token are refused (`AUTH-002`, `AUTH-003`); `GET /api/login` answers 405 (`AUTH-004`); the supplied credentials log in (`AUTH-005`); the session cookie is `HttpOnly`, has `SameSite` and, over HTTPS, `Secure` (`AUTH-007`–`009`); after the run's closing logout the old cookie answers 401 (`AUTH-010`). | `AuthMiddleware` (`auth.go`), `handleLogin`, `handleLogout` |
| `auth` | aggressive | `AUTH-006`: a wrong password for the supplied username and for an unknown one must answer the same status and body. | `ValidateCredentials` |
| `csrf` | default | `POST /api/login` without `Origin` or `Referer` (`CSRF-001`); with a foreign `Origin`, and `POST /api/logout` with only a foreign `Referer` (`CSRF-002`): 403, else a WARN (`-expect origin-validation`). With a session, a same-origin `POST /api/logout` without `X-CSRF-Token` (`CSRF-003`) and with a token wrong in its last character (`CSRF-004`): 403. One that is accepted fails and has ended the scan's session, so later checks that need one skip. | `CSRFMiddleware`, `ValidateOrigin` (`auth.go`) |
| `cors` | default | A preflight `OPTIONS /api/current` from a random foreign origin gets no grant (`CORS-001`): reflecting it, or a wildcard with credentials, is critical; a wildcard without credentials is a FAIL with auth off (any site reads the API) and a WARN with auth on. `Vary: Origin` whenever `Access-Control-Allow-Origin` is sent (`CORS-002`). `null`, trailing-dot, userinfo and look-alike forms of the target's own origin are never reflected, even when it allow-lists itself (`CORS-003`). | `corsMiddleware`, `isAllowedOrigin` (`server.go`) |
| `host` | default | `HOST-001`: raw `GET /api/config` (auth off) or a token-less `/metrics` (auth on) under a random `*.example.com` name, with a port, a trailing dot, in mixed case and behind a `localhost.` label. Kula's 403, a proxy's refusal, or a close or reset without a response passes; Kula's data under any of them is a WARN (`-expect host-check`); no answer within `-timeout`, under the scanner's own name or any foreign one, is an ERROR. SKIP on a Unix socket, and with auth on when `/metrics` asks for a token or is off. | `hostCheckMiddleware` (`host_check.go`) |
| `traversal` | default | Over a raw socket, so nothing normalises the path: `/style.css` must answer 200 (`TRAV-000`); eight encoded, dot-dot and backslash payloads must return no file content from outside the static directory (`TRAV-001`); `/js/` and `/fonts/` no directory listing (`TRAV-002`). | `handleStatic` |
| `metrics` | default | `/metrics` without a token: 401 passes (`PROM-001`), 200 is a WARN (`-expect metrics-token`), 404 a SKIP. A wrong bearer token answers 401 (`PROM-002`). With `-metrics-token`, the right one answers 200 with a Prometheus exposition (`PROM-003`); a refused token is an ERROR. | `handleMetrics` (`prometheus.go`) |
| `ws` | default | With auth on, an upgrade without a session answers 401 (`WS-001`). An upgrade from a foreign `Origin` (with the session behind auth) answers 403 (`WS-002`); one that succeeds is a FAIL with auth off, and with auth on is judged by the session cookie: `SameSite=Strict` or `Lax` keeps other sites out, a WARN (`-expect origin-validation`); `None` or no attribute is a FAIL. A same-origin upgrade succeeds (`WS-000`, the control). | `handleWebSocket`, `newUpgrader` (`websocket.go`) |
| `ws` | aggressive | `WS-FLOOD`: 12 upgrades must start answering 429, naming the per-address or global cap that answered. `WS-MSGBOMB`: a well-formed 64 KiB JSON command (a resume command padded with whitespace), then a ping. The connection closed passes (Kula's read limit is 4 KiB). A pong with the connection still open 1 s later (or `-timeout`, if shorter) shows the message was read, a HIGH FAIL; neither within `-timeout` is a WARN. | `max_websocket_conns(_per_ip)`, `SetReadLimit` |
| `input` | default | Needs the API (auth off, or a session). `/api/history` answers 400 to a malformed `from` or `to`, an inverted or over-31-day range, a negative or non-numeric `tier` and an unknown section (`INPUT-001`). `points=999999999` over the last two hours returns at most 5000 samples, counted as they stream, once those hours hold more than 5000 stored records (`INPUT-002`; below). `/api/i18n` with traversal and junk `lang` values reads no file (`INPUT-003`). | `handleHistory`, `validateHistoryRange`, `parseHistorySections`, `handleI18n` |
| `input` | aggressive | `INPUT-AGG`: an 8 KiB login body is refused (400/413) by the 4 KiB cap; SKIP with auth off. `INPUT-CHAT`: a 320 KiB chat body over a raw socket, its last bytes held back, must be refused before it is complete (Kula reads 32 KiB); an answer only after the whole body is a WARN (no cap, or a proxy that buffers bodies). | `MaxBytesReader` in `handleLogin`, `handleOllamaChat` |
| `redirect` | default | `REDIR-001`: eleven crafted paths (`//evil.example/`, `/%2f%2f…`, `/%5c%5c…`, `https:/…`, and two around the base path when there is one) must not produce a 3xx whose `Location`, read as a browser reads it (control characters stripped, backslashes as slashes), names another host. | base-path redirect (`mountWithBasePath`) |
| `integrity` | default | `SRI-001`: every script and `modulepreload` of the UI page is same-origin with a sha256/384/512 `integrity`, and every inline script carries the CSP nonce. `SRI-002`: each such script, fetched and content-decoded as a browser does, hashes to its strongest listed hash; a mismatch names whether the page's hash or the script changed (from Kula's `ETag`). Catches a proxy or CDN that rewrites or injects scripts. SKIP when the UI is off. | `calculateSRIs` (`server.go`) |
| `tls` | default | HTTPS only; SKIP over HTTP. Handshakes capped at TLS 1.1 and at TLS 1.0 must be refused, by an alert or a close or reset after the ClientHello; an answer that is not TLS is no refusal (`TLS-001`). A TLS 1.2 handshake offering only the suites Go lists as insecure, of those the scanner's client offers (below), must be refused; each accepted suite is named, RC4 or 3DES a FAIL, the rest a WARN (`TLS-002`). The leaf is valid now and for 14 more days, with no RSA key under 2048 bits and no SHA-1 signature in the chain (`TLS-003`). The chain leads to a system root and names the host (`TLS-004`; with `-insecure` the scanner verifies it itself, a failure is a WARN for `-expect tls`). | the TLS terminator (Kula itself only serves HTTP) |
| `rate` | aggressive | `RATE-LOGIN`: 8 failed logins for one account must meet a 429; all reaching the credential check is a FAIL. `RATE-OLLAMA`: 70 `GET /api/ollama/models` must meet a 429 (60 a minute). `RATE-CHAT`: up to 14 chat requests whose bodies never decode must meet a 429 (10 a minute). The AI probes use the session behind auth and SKIP when the assistant is off. | `RateLimiter` (`auth.go`), `chatRateLimiter` (`ollama.go`) |
| `dos` | aggressive | `DOS-SLOWLORIS`: 12 half-open requests; `/health`, asked on a new connection, must keep answering and all must be dropped within `-dos-wait`. `DOS-HEADERBOMB`: a 2 MiB header block must be refused: 431, the 400 or 413 a proxy answers, or the connection closed; 408, 429 and 503 refuse it for another reason, an ERROR; any other answer (2xx, 3xx, 404, 5xx) was given after the whole block was read, a HIGH FAIL. `DOS-CONNFLOOD`: 64 idle connections; `/health`, on a new connection, must answer and all must be dropped within `-dos-wait`. `DOS-HISTORY`: 8 concurrent 31-day, 5000-point history requests must each be answered with data or deferred with 503 and `Retry-After`, none hanging, while `/health` keeps answering; a burst not wholly sent is an ERROR. | `ReadTimeout`, `IdleTimeout`, `MaxHeaderBytes`, history slots (`history_response.go`) |
| `bypass` | aggressive | `BYPASS-XFF`: 12 failed logins, each with a different spoofed `X-Forwarded-For`, must still be throttled (429); 11 or more reaching the credential check means the limiter trusts the header (`trust_proxy` on without a proxy). | `getClientIP` (`server.go`) |
| `fuzz` | fuzz | Blind fault injection; see [Fuzzing](#fuzzing--fuzz). | the whole surface |

The CSP (`HDR-003`, `HDR-007`, `HDR-011`–`HDR-013`) is parsed by directive, every policy of
every `Content-Security-Policy` header, and a restriction any enforced policy makes counts:

- `HDR-003` (FAIL): `default-src` is `'self'` or `'none'`, and the effective `script-src` has a
  nonce and no `'unsafe-inline'`, `'unsafe-eval'`, wildcard host (`*`, `https://*`,
  `*.example.com`) or bare scheme (`https:`, `data:`).
- `HDR-007` (WARN, `-expect frame-protection`): `frame-ancestors 'none'`.
- `HDR-011`, `HDR-012` (WARN): `base-uri` and `form-action` are `'self'` or `'none'`; neither
  falls back to `default-src`.
- `HDR-013` (WARN): the effective `object-src` (else `default-src`) is `'none'`.

No `Content-Security-Policy` at all fails `HDR-003` and warns on the other four.

`HDR-002` passes when the UI page carries `X-Frame-Options: DENY`. Missing while the other
security headers are sent (`web.security.frame_protection: false`), it is a WARN for
`-expect frame-protection`; missing with them (`web.security.headers: false`), or set to any
other value, it is a FAIL.

`AUTH-001` sends, without a session: `GET /api/current`, `/api/history`, `/api/config`,
`/api/i18n?lang=en`, `/api/system-info`, `/api/ollama/models`, `/api/ollama/context`, a
same-origin `POST /api/ollama/chat` with no body, and `GET` on a random unknown `/api/` path.
Every one must answer 401, whatever features are enabled.

`AUTH-010` costs nothing extra: the run logs out at the end anyway, and replays the logged-out
cookie on `/api/config`. It is a SKIP when the run had no session or was interrupted.

`INPUT-002` shows the cap only when the window holds more stored records than the cap: below
it, a server without the cap answers every record too. It counts them from the samples'
`sample_count`, which Kula 0.20.0 and later send, and is a SKIP ("cap not reached") while the
last two hours hold 5000 records or fewer, and a SKIP too when the samples carry no
`sample_count`. At the default one-second interval the window holds more after about 84 minutes
of collection. A longer window would not help: Kula reads it from a coarser tier and answers
fewer points than the cap either way (31 days: 10-minute buckets, at most 4465).

Every WebSocket probe but `WS-FLOOD` (`WS-000`–`WS-002`, `WS-MSGBOMB`, `FUZZ-WS`) ends each
connection with a close frame and waits up to 2 s (or `-timeout`, if shorter) for the target to
end it, by which time Kula has freed the connection's place under
`web.max_websocket_conns_per_ip`; an upgrade refused with 429 is tried once more after 500 ms.
So they fit within a limit of one connection per address, directly or through a proxy.

`TLS-002`'s insecure list is `crypto/tls`'s `InsecureCipherSuites()` in the Go toolchain the
scanner was built with. Under current Go it includes the RSA key-exchange suites
(`TLS_RSA_WITH_AES_128_GCM_SHA256` and the like), which nginx's default `ssl_ciphers` enables, so
a stock nginx gets a `TLS-002` WARN. All TLS probes go through `-connect` and `-unix` too.
`TLS-002` probes only the insecure suites the scanner's own TLS client really offers: it is an
ERROR when the client offers none of them, or leaves some out and the server accepted none of
the rest. In FIPS 140 mode (`GODEBUG=fips140`) that client builds no handshake below TLS 1.2 and
offers only approved suites: a probe it cannot make asks the target nothing, so `TLS-001` and
`TLS-002` are an ERROR whose detail names the mode.

The slow-connection probes (`DOS-SLOWLORIS`, `DOS-CONNFLOOD`) judge whoever holds the socket.
When connections outlive `-dos-wait` and the responses carry `Server` or `Via` (Kula sends
neither), the finding says a reverse proxy likely answers, whose own timeouts apply (nginx and
Caddy wait 60 s by default), and suggests a longer `-dos-wait` such as `75s`. A `DOS-SLOWLORIS`
that saw none of its half-open requests dropped is then a MEDIUM WARN, not a HIGH FAIL, while
`-dos-wait` is under 75 s: rerun with `-dos-wait 75s` or scan Kula directly (`-connect` to its
own address).

## Fuzzing (`-fuzz`)

The categories above ask whether a known defense holds; `-fuzz` throws malformed and extreme
input at the surface and watches for what must never happen. The **anomaly oracle** flags:

- an unexpected **5xx**: a handler erred where a 4xx belonged;
- a **dropped connection** (EOF, reset) on a request: typically a recovered handler panic;
- a **hang**: no answer within `-timeout`;
- **reflected input**: a `<kx…>` canary echoed unescaped into an HTML or JavaScript response;
- the target **refusing connections** it took before, and **`FUZZ-LIVENESS`**: `/health` must
  still answer at the very end of the run.

Not anomalies: 4xx answers; `net/http`'s own refusal of a request it cannot parse (a
`text/plain` 400, 431 or 505, or 501 for an unknown `Transfer-Encoding`), counted as reaching the
server; Kula's auth, CSRF and Host gates; 429s; and the 5xx answers Kula gives on purpose, each
matched by route, JSON error text and `Retry-After`: `/api/current` before the first sample
(503), `/api/history` past its two concurrent queries (503 with `Retry-After`), login when no
password-hashing slot is free (503 with `Retry-After`), the AI routes with the assistant disabled
(503) and `/api/ollama/models` when the AI backend is down (502).

Each probe ends with a coverage line, e.g. `200 planned, 200 sent: 200 reached the history and
i18n handlers (28 of them refused by the HTTP parser before routing)`, also counting requests
stopped at a gate, throttled, answered unavailable or not sent. The verdict follows it: an
anomaly is a FAIL; nothing reaching the code under test is a SKIP when a limiter or a disabled
feature turned the requests away and an ERROR otherwise; fewer than half the planned requests
reaching it is a WARN.

| Probe | What it sends |
|-------|---------------|
| `FUZZ-QUERY` | `-fuzz-iter` requests to `/api/history` (`from`, `to`, `points`, `tier`, `sections`, valid, nearly valid and hostile) and `/api/i18n` (`lang`); half through the HTTP client, half written raw with bad escapes, control and high bytes and odd separators. Needs the API. |
| `FUZZ-PATH` | `-fuzz-iter` hostile paths with a reflection canary, half escaped, half raw. |
| `FUZZ-BODY` | At most 6 malformed JSON bodies to `/api/login` (with auth on) and 6 to the AI chat (when enabled; with the session behind auth), each series stopping at a 429. Every chat body is one the chat's decoder refuses, so no chat starts. |
| `FUZZ-METHODS` | Unusual methods (`PUT`, `DELETE`, `PATCH`, `TRACE`, `PROPFIND`, `MKCOL`, `BREW`, `FOObar`), `HEAD` and `OPTIONS` on 17 routes, and `POST` on the 8 read-only API routes: 178 requests, past the auth and CSRF gates. A read-only API route accepting `POST`, `PUT`, `DELETE` or `PATCH`, or login, logout or the chat accepting anything but `POST` and `OPTIONS`, is an anomaly. `POST` is never sent to those three, nor `CONNECT` to any route (a reverse proxy answers it itself). Outside `/api/` Kula serves its page, assets, `/health` and `/status` to any method; those 2xx are information only. |
| `FUZZ-SMUGGLE` | Three ambiguously framed `POST /api/login` over raw sockets: two `Content-Length`s, `Content-Length` with chunked `Transfer-Encoding`, two `Transfer-Encoding`s. Each must be refused (a close without a response counts), or handled without a 2xx or a 500; the detail says what each was answered. One the target neither answers nor closes within `-timeout` shows nothing: with no 2xx or 500 among the others, an ERROR. |
| `FUZZ-WS` | Six same-origin WebSocket connections in turn (with the session behind auth), up to 8 malformed, binary and pause/resume frames on each, each connection closed before the next opens; then HTTP and a new upgrade must still work. Opening no connection is not a pass. |
| `FUZZ-LIVENESS` | `GET /health` (up to 3 tries) after everything else, the lockout probes included. |

Every probe draws from one seeded PRNG. The seed is in the `-fuzz` warning, the evidence of the
randomized probes and the JSON `options.seed`; rerun with `-seed N` and the same `-fuzz-iter`
and selection to send the same randomized inputs again:

```bash
kula-scan -fuzz -fuzz-iter 500 -username admin http://localhost:27960
kula-scan -fuzz -seed 12345 -only fuzz http://localhost:27960
```

## What a run costs the target

**Default run.** It reads headers and pages and sends single-shot probes. With credentials it
logs in once and logs out at the end; it never sends a wrong password for the supplied account.
Kula counts every login against 5 per account and address and 15 per address in five minutes,
so five authenticated default runs fit in that window. The integrity checks fetch the UI page and
each script it names (about 40 requests on a stock Kula). With `origin_validation` off,
`CSRF-001` and `CSRF-002` reach the login handler with a made-up account and spend two of the
address's attempts.

**`-fuzz`** sends about 600 malformed requests at the default `-fuzz-iter` (two probes send
`-fuzz-iter` each), and they stay in the target's access log. With auth on it spends up to 6 of
the address's 15 login attempts in five minutes (7 with origin validation off). It never starts
a model chat: every chat body it sends fails Kula's decoder.

**`-aggressive`** has real side effects, and the scanner prints a warning first:

- It exhausts the per-address login limit on purpose (`RATE-LOGIN`, `BYPASS-XFF`): for about
  five minutes no login from the scanner's address succeeds, neither a second scan's (its
  `AUTH-005` is an ERROR, exit 3) nor that of any user behind the same address or NAT.
- `AUTH-006` spends one more of the supplied account's attempts.
- It briefly fills the address's WebSocket slots, spends the address's share of the AI limiters
  for a minute (without starting a chat), holds 12 slow and 64 idle connections up to
  `-dos-wait`, sends a 2 MiB header block and loads the store with 8 widest-window history
  queries at once.
- With the AI assistant on, `RATE-OLLAMA`'s 70 requests for the model list make Kula ask its
  configured backend for it about 60 times.
- A full `-aggressive -fuzz` run takes about a minute, most of it the two `-dos-wait` waits.

Run `-aggressive` against staging, or accept the lockout on production.

### Run order and login budget

The default checks run first, by category, then most aggressive checks, then the fuzz checks;
the login and chat budgets order the end of the run. Every
`POST /api/login` that passes the CSRF gate counts against the address's 15 attempts in five
minutes, whatever its body; one whose body decodes also counts against that account's 5. The
probes that spend attempts run first, so each still reaches the code it tests:

| Attempts | By |
|---------:|----|
| 1 | the scan's login, by the first check that needs a session |
| 2 | `AUTH-006`: a wrong password for the known and an unknown username |
| 1 | `INPUT-AGG`: an 8 KiB body, refused by the 4 KiB cap |
| ≤ 6 | `FUZZ-BODY`: malformed bodies, stopping at a 429 |
| **≤ 10** | of 15 |

Then the two that exhaust the limiter on purpose: `RATE-LOGIN` sends 8 for one account, 5 reach
the handler and 3 are refused (its PASS); `BYPASS-XFF`'s 12 then all find the address spent (its
PASS, with `trust_proxy` off). `RATE-CHAT` runs after `FUZZ-BODY`, which sends the chat bodies of
its own. `FUZZ-LIVENESS` ends the run.

With `origin_validation` off, three more reach the limiter before `RATE-LOGIN`: `CSRF-001`'s and
`CSRF-002`'s logins, and the one of `FUZZ-SMUGGLE`'s requests that `net/http` does not refuse
before routing. That is 13 (at most 15) before `RATE-LOGIN`, and every judgement still holds.

## What it cannot tell

- **One server's view.** `FUZZ-SMUGGLE` shows how the answering server reads ambiguous framing.
  A request-smuggling desync is a disagreement between a proxy and the server behind it, which
  one server's answers cannot show; scan through the proxy and, with `-connect`, Kula directly.
- **The front door decides some results.** Behind a reverse proxy, the TLS, slow-connection,
  header-bomb and body-cap results describe the proxy as much as Kula.
- **Cipher suites move with Go.** `TLS-002` judges against the insecure list of the toolchain the
  scanner was built with, and only the suites its client offers; in FIPS 140 mode the scanner
  cannot make the `TLS-001` and `TLS-002` probes at all.
- **A young store hides the history cap.** `INPUT-002` is a SKIP until the last two hours hold
  more than 5000 stored records: at one-second sampling, about 84 minutes after a fresh store
  starts collecting.
- **Only what is served is judged.** With `web.ui: false`, or behind a proxy that forwards only
  `/health`, `/status` and `/metrics`, the checks that need the UI or API are SKIPs: a clean
  run says nothing of the safeguards behind them.
- **Fuzzing is sampled.** A PASS says the requests it sent found nothing, under that seed.

## Development

Architecture, the catalog and registry, the judging rules a new check must follow, and the tests
are in [kula-scan (developer notes)](../../docs/dev/13-kula-scan.md). Among them,
[`integration_test.go`](integration_test.go) runs this scanner against the real `kula serve` in
ten postures, and the [Docker showcase](../../addons/docker/showcase/README.md) scans a full
deployment directly and through a TLS proxy.
