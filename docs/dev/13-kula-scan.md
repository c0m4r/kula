# kula-scan

Source: [`cmd/kula-scan/`](../../cmd/kula-scan/) · Reference:
[`cmd/kula-scan/README.md`](../../cmd/kula-scan/README.md)

`kula-scan` is a black-box safeguard scanner for a running Kula instance: it probes a live URL
over HTTP, raw sockets, TLS and WebSocket and reports per finding whether each safeguard holds
in that deployment. The [README](../../cmd/kula-scan/README.md) is the operator's reference,
and the only page that lists flags, statuses, exit statuses, `-expect` keys, report formats,
what each category sends and judges, and what a run costs the target. This page is how the
scanner is built, the rules a check follows, and how to add one.

In brief: a separate binary in this module (so `go vet ./...` and `go test ./...` cover it),
built by `./addons/build.sh` and shipped in each release as
`kula-scan-linux-<version>-<arch>.gz`, outside the packages and the Docker image
([Release orchestration](15-packaging.md#release-orchestration)). It imports nothing from
`internal/`, only the root `kula` package for its version: every judgement is made over the
wire. Exit status 0 is clean, 1 a gating FAIL or an unmet `-expect`, 2 a usage error or a target
that is unreachable or not Kula, 3 inconclusive (an ERROR), 130 interrupted.

## Files

| File | Holds |
|------|-------|
| `main.go` | Flags, `printUsage`, the `-aggressive` and `-fuzz` banners, `runCLI` (stdout gets the report and nothing else) |
| `catalog.go` | `catalog`: every finding ID with its category, mode, title and description; `-list` |
| `expect.go` | `expectKeys` and `applyExpectations` (`-expect`) |
| `selection.go` | `-only`, `-skip`, `-ignore` |
| `scanner.go` | `Scanner`, `NewScanner`, the transport helpers, `discover`, `ensureSession`, `allChecks`, `Run`, the closing logout |
| `headers.go` | `-H` parsing, the refused names, adding the headers to every request |
| `util.go` | `printable` (terminal-safe text), small helpers |
| `report.go`, `report_format.go` | `Report`, the text and JSON formats, `exitCode`, `exitReason`, `-o` |
| `report_junit.go`, `report_sarif.go` | The JUnit and SARIF formats |
| `checks.go` | headers, auth, csrf, cors, traversal, metrics, input |
| `checks_host.go` | `HOST-001`, `DISC-003` and the refused-`Host` rules (`runsUnderRefusedHost`) |
| `checks_ws.go`, `checks_sri.go`, `checks_tls.go` | WebSocket gates; subresource integrity (with its own HTML tag scanner); TLS |
| `checks_bypass.go` | `REDIR-001`, `BYPASS-XFF` |
| `checks_aggressive.go`, `checks_dos.go`, `checks_limits.go` | The aggressive probes: rate limits, WebSocket flood, DoS, AI chat and history limits |
| `checks_fuzz.go` | The fuzz probes and the anomaly oracle |

## A run

1. `runCLI` parses every flag before anything is sent: an unknown category, ID, `-expect` key or
   format, a bad `-H`, `-connect` or base path is exit 2. `NewScanner` folds a URL path into the
   base path and decides where every connection goes (`dialTarget`: the URL's address,
   `-connect` or `-unix`). With `-o`, the file is opened now.
2. `Run` narrows `allChecks()` to the selection: a check runs when any ID it declares is
   selected, and reports only those.
3. `discover` reads `/health`, `/api/auth/status` and `/api/config` and emits `DISC-001`
   (reachable and Kula, or an ERROR that ends the scan), `DISC-002` and `DISC-003`. A refused
   `Host` name leaves only the checks `runsUnderRefusedHost` names; the rest are SKIPs.
4. Each check in registry order: one of a mode not enabled reports a SKIP per ID
   (`skippedFindings`); the others run, and `applyExpectations` turns a WARN tagged with a
   declared `-expect` key into a FAIL.
5. `closeSession` logs out the session `ensureSession` opened; when the auth check ran, that
   logout is also the `AUTH-010` probe. A deferred logout covers a check that panics.
6. `markIgnored`, `newReport` and `exitCode` (checked in the order 130, 2, 1, 3) produce the
   report; `exitReason` explains a non-zero exit on stderr.

An interrupt cancels the run's context: requests in flight abort (`dialRaw` and `wsDial`
connections are closed through `closeOnCancel`), the check cut short loses its findings, no
further check starts, and the logout still goes out under a fresh 5-second context.

## Catalog and registry

A finding's ID is its stable name: `-only`, `-skip`, `-ignore`, baselines and SARIF rules all
refer to it. `finding(id, severity, status, detail)` takes the category and title from the
catalog, so the title never varies with the outcome; what varies goes in the detail, evidence
and remediation. A `check` in the registry is `{ids, run, hint}`: `ids` are every ID it can
emit, and they agree on category and mode, which the runner reads from the first. Discovery's IDs
come from `discover`, not from a check.

Three tests hold this together:

- `TestCatalogCoversEveryEmittedID` parses the package's sources for every string passed as
  the ID to `finding()` (or to a helper that forwards its first parameter to it) and requires
  each to be in the catalog, and each catalog entry to be emitted.
- `TestRegistryDeclaresEveryCatalogID` requires every non-discovery ID to be declared by exactly
  one registry check, with one category and mode per check.
- `TestCatalogEntriesAreComplete`: IDs are upper case with a hyphen, unique, and never collide
  with a category name (both share the `-only` namespace).

## Judging rules

What makes the scanner usable as a gate is that its statuses mean the same thing everywhere:

- **A PASS needs an answer that shows the safeguard.** Requests that failed prove nothing: a
  probe whose requests all failed is an ERROR, never a PASS. `undelivered` counts raw requests
  that never reached the target, `tally` the answers of a burst, `fuzzTally` the fate of each
  fuzz request; a probe with some answers says how many failed (`unreachedNote`). A burst that
  got no 429 because its requests failed is an ERROR, not a FAIL either.
- **A correct Kula never FAILs.** What an operator may turn off (auth, origin validation, frame
  protection, the `/metrics` token, the Host check, HTTPS) is a WARN tagged with
  `expecting(key)`, which `-expect key` turns into a FAIL. A new posture needs its key in
  `expectKeys`. A check with no WARN to upgrade asks `s.expects(key)` itself (`HDR-008` and
  `TLS-001` over plain HTTP).
- **A SKIP says why**: `noSessionReason`, `refusedHostReason`, the feature that is off.
- **Another gate's answer is not the safeguard's.** Kula's Host-check refusal is recognised by
  its JSON error text (`hostCheckRefusal`); under it (`DISC-003`) the checks that would read its
  403 as their own PASS are skipped. The fuzz oracle tells the auth and CSRF gates' 401 and 403
  from the code under test the same way (`gateRefusal`), so a request stopped at a gate does not
  count as coverage.
- **One session per run.** A check that needs one calls `ensureSession()`, which logs in once,
  lazily, and never retries. Besides that login, `AUTH-006`, `INPUT-AGG`, `FUZZ-BODY`,
  `RATE-LOGIN` and `BYPASS-XFF` post to `/api/login` on purpose; `CSRF-001`, `CSRF-002` and
  `FUZZ-SMUGGLE` send it requests that the CSRF gate or `net/http` refuse, and that reach the
  limiter only with origin validation off. A probe that ends the session (a CSRF token gate that
  accepts a logout) calls `markSessionLost`, so later checks skip instead of misreporting.
- **No chat starts.** Nothing posts a body that decodes to `/api/ollama/chat`: one that decodes
  takes a chat slot and calls the model. The chat probes send JSON that never decodes.
- **Every connection goes through the scanner's helpers** (`do`, `doContext`, `rawTCP`,
  `rawExchange`, `rawRequest`, `hostRequest`, `dialRaw`, `dialTLS`, `wsDial`), so `-connect`,
  `-unix`, `-H`, the base path, `-timeout` and the interrupt apply. Never `http.Get` or a client
  of its own.
- **What the target sends is bounded**: status line and headers at 2 MiB (`readResponse`,
  `MaxResponseHeaderBytes`, a byte budget on the WebSocket handshake), bodies and WebSocket
  messages at 1 MiB. The text and JUnit reports write every quoted field through `printable`,
  and the JSON and SARIF encoder escapes the same characters, so a hostile target can neither
  grow the scanner's memory nor rewrite the report on a terminal.

## Transport

The HTTP client has no cookie jar (cookies are set per probe) and does not follow redirects, so
301, 401 and 403 are observed directly. It dials through `Scanner.dial`, whatever the URL names,
and its TLS server name is always the URL's host. Traversal, path and query fuzzing, request
smuggling, `HOST-001`, `INPUT-CHAT` and the DoS probes write their requests by hand over a raw
TCP or TLS connection, so nothing normalises the path or the framing; `rawHeaderLines` adds the
`-H` headers to them. WebSocket probes use `gorilla/websocket`, the server's own library. The
TLS probes (`tryHandshake`) offer one version or suite set per handshake and send no data.

## Run order and the login budget

`allChecks()` runs the categories in catalog order, then orders the end of the run by the login
budget, which the comment there spells out and the README's
[Run order and login budget](../../cmd/kula-scan/README.md#run-order-and-login-budget) explains:
every probe that posts to `/api/login` runs before `RATE-LOGIN` and `BYPASS-XFF`, which exhaust
the address's 15 attempts on purpose; `RATE-CHAT` runs after `FUZZ-BODY`; `FUZZ-LIVENESS` is
last. `TestLoginBudgetOrder` checks the order, `TestAggressiveFuzzRunKeepsTheLoginBudget` that
every judgement still holds when the attempts are spent, with origin validation on and off,
`TestDefaultRunLogsInOnceAndOut` that a default run logs in once and out, and
`TestFuzzBannerStatesTheLoginCost` the `-fuzz` banner's numbers. A probe that starts spending
login attempts goes before the lockout probes, and its cost goes into that comment and the
README table.

## The fuzz oracle

`classify` judges each fuzz request as reached (the code under test answered), gated, throttled,
unavailable, unsent, failed or an anomaly (unexpected 5xx, a dropped connection, a hang).
`deliberate5xx` lists the 5xx answers `internal/web` gives on purpose, matched by status, route,
JSON error text and code, and `Retry-After`; a new deliberate 5xx in Kula needs an entry there,
or `-fuzz` reports it as an anomaly. `parserRefusal` recognises `net/http`'s own refusals before
routing, and `gateRefusal` Kula's gates. `fuzzTally` counts each probe's requests into its
coverage line, and `verdict` turns it into the finding (FAIL on an anomaly; SKIP or ERROR when
nothing reached the code; WARN under half; else PASS).

## Adding a check

1. Add the ID to `catalog` in its category, with mode, a title that holds for every outcome and
   a one-line description of what it verifies. A new category goes where it runs.
2. Declare it in the `ids` of exactly one check, in its category's registry function, and put a
   new check where it belongs in `allChecks()` (login budget, limiters it spends).
3. Follow the judging rules: PASS only on an answer, ERROR when the requests failed, a WARN with
   `expecting(key)` for an operator choice, a SKIP that says why, and a remediation that names
   the config key or the Kula version.
4. Test it against a mock that holds the safeguard and one that does not: `secureMock` and
   `insecureMock` (`checks_test.go`), or `kulaMock` (`fuzz_mock_test.go`), which emulates
   `internal/web`'s routes, method checks, gates, limiters and body decoding.
5. Add it to the integration test's expectation tables, the exact WARN and SKIP sets of each
   posture it runs in (see [Testing](12-testing.md#black-box-scanning-kula-scan)).
6. Document it in the README's category table, and in `-expect`'s table when it adds a key.

A new safeguard, route or limit on the server side needs the same: a check here and its line in
the integration test, or the scanner silently stops covering the surface.

## Tests

Every check is unit-tested against mock servers in this package (`go test ./cmd/kula-scan`):
the secure and insecure mocks, `kulaMock` for the fuzz and limit probes, and targeted tests for
each transport path, report format, exit status and the interrupt. The integration test boots
the real `kula serve` and runs the scanner against it; the showcase runs it against a deployment
behind a base path and Caddy. Both are described in
[Testing](12-testing.md#black-box-scanning-kula-scan).

Next: [Adding a Metric Type](14-adding-metrics.md).
