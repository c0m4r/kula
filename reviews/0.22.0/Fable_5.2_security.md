# Kula 0.22.0 — Security audit

| | |
|---|---|
| **Commit** | `6bd595f` (branch `main`; tree clean apart from the untracked `reviews/0.22.0/`) |
| **Version** | `VERSION` = `0.22.0` |
| **Date** | 2026-11-11 |
| **Reviewer** | Claude Fable 5.2: web server, auth, Host check, WebSocket, AI proxy, sandbox and startup first-hand. The rest by five parallel auditors (Claude Opus 5.5): collectors, storage, frontend, Prometheus/TUI/kula-scan, supply chain. |
| **Changes** | None by the audit itself: no repository file was modified and nothing was committed. Fixes followed in separate commits, all released in 0.22.0. |
| **Status** | Fixes released in tag `0.22.0` (2026-11-14): H1 and all Medium findings fixed, 14 of 16 Low fixed, L3 and L14 in part. Each finding carries its status, a fixed one marked ✅; §8 has the commit map, the decisions and what is still open. |

This is separate from the pre-release review in `Fable_5.2.md`; items that review lists as known
or fixed are not repeated.

Evidence labels used below:

- **First-hand** — run or read by the lead reviewer.
- **Reproduced** — an auditor ran it and quoted the output; not re-run by the lead unless it says so.
- **By reading** — from the code only.

Severity is for the default deployment (all interfaces, port 27960, auth off, sandbox on,
containers collector on, everything else off) unless a finding names what else it needs.

---

## 1. Verdict

No authentication bypass, no Host-check or CSRF bypass, no cross-site WebSocket hijack and no XSS
was found. The request-handling core holds up, and the project's own scanner passes.

The weaknesses are elsewhere: resource bounds on what the network and the host can make Kula hold,
containment once something goes wrong, and packaging. One finding is High and seven are Medium.

| ID | Severity | Finding | Needs | Evidence | Status |
|---|---|---|---|---|---|
| H1 | High | Ten unauthenticated history requests take 4 GB of memory | default deployment, a few days of history | First-hand | ✅ **Fixed** |
| M1 | Medium | Any local user can plant "containers": history retention drops from days to minutes | no reachable container runtime socket | Reproduced; mechanism confirmed first-hand by reading | ✅ **Fixed** |
| M2 | Medium | Sandbox pinned to Landlock ABI 5: Unix sockets, UDP and signals are not confined | code execution in the process | First-hand | ✅ **Fixed** (MPTCP, SCTP, ICMP open, §8.5) |
| M3 | Medium | A fake PostgreSQL/MySQL server crashes Kula with one packet and gets the monitoring password | postgres or mysql collector on | Reproduced | ✅ **Fixed** |
| M4 | Medium | A root `kula tui` opens paths chosen by uid 1000 | root TUI, no rootful runtime socket | Reproduced (not as root) | ✅ **Fixed** |
| M5 | Medium | `verify-lock.js` accepts a lockfile that installs a different package | a lockfile change reaching the maintainer | Reproduced; check confirmed first-hand by reading | ✅ **Fixed** |
| M6 | Medium | The `.deb` and the snap leave `config.yaml` world-readable | Debian/Ubuntu or snap install | Reproduced on the 0.21.0 `.deb` | ✅ **Fixed** |
| M7 | Medium | Release tarballs carry uid 1000; a root extraction hands `/opt/kula` to that uid | the installer's tarball fallback | First-hand (tarball); extraction reproduced | ✅ **Fixed** for new releases |

Sixteen Low findings and a list of hardening items follow in §3. The audit did not establish which
findings are new in 0.22.0; where an auditor dated one, it says so. Of the Low findings, 14 are
fixed and L3 and L14 are fixed in part.

## 2. Scope and method

**Attackers considered:** an unauthenticated network client; a malicious page in the operator's
browser; an unprivileged local user or a container on the monitored host; a hostile monitored
service (status endpoint, database, container daemon, AI backend); someone who hands the operator a
tier file, a backup or a scan target; a network intermediary or dependency publisher at build and
install time.

**What was run.** Loopback instances of `kula serve` built from HEAD, with auth off and on, probed
by hand and with `kula-scan`; a fake AI backend; a probe that applies Kula's own `sandbox.Enforce`
and tests what the process can still reach; three days of mock history for the memory measurement.
The auditors added fake Docker, PostgreSQL, MySQL and Ollama servers, headless Chromium against the
real server, fuzzing of the tier-open path, and crafted lockfiles and tier files. The supply-chain
audit was static: no install, build or release script was executed.

## 3. Findings

### High

**H1 — Unauthenticated history requests exhaust memory.**
`internal/web/server.go:815-899` (no limit on concurrent requests), `:321-330` (the response is
encoded in memory); `internal/storage/store.go:827-845` (every result is cloned into a cache capped
at 256 entries, not bytes), `:855-949`, `history_stream.go:11-116` (up to 5000 buckets, each a full
Data+Min+Max sample).

Any client repeats `GET /api/history?from=<now-72h>&to=<now-N s>&points=5000`, varying `to` so no
two requests share a cache entry. Memory grows linearly with the number in flight.

*First-hand*, three days of mock data, default tiers, auth off:

| Requests in flight | Peak RSS |
|---|---|
| idle | 22 MB |
| 1 (200, 89.7 MB body, 0.42 s) | 377 MB |
| 5 | 1.88 GB |
| 10 (about 2 s each) | 4.35 GB |

The storage auditor measured 3.77 GB at 10 and 10.6 GB at 30. The systemd unit sets no `MemoryMax`,
so the pressure lands on the monitored host and the kernel chooses what to kill. The auditor rated
this Medium; it is High here because it needs no credentials, ten requests and nothing unusual on
the host. With auth on it needs a session. Queries do not stall the writer (slowest `WriteSample`
4 ms under eight query loops), so this is memory only.

*Fix:* a semaphore around the history query in the handler (2–4 in flight, the rest 429/503); skip
the cache above a byte budget; an element budget per result that falls back to a coarser step;
`MemoryMax` in the unit as a backstop.

> ✅ **Status: fixed** (`3f8c8f4`, `41392b2`, `5a50a96`, `c12cb30`, `d74ddf0`). History queries run two
> at a time and stream their response instead of encoding it in memory; one that waits too long
> answers 503, and the dashboard retries. The unit sets `MemoryMax=1G` and `GOMEMLIMIT=512MiB`.
> Re-measured with three days of history at full width: about 340 MB with 10 requests in flight and
> 470 MB with 30 on a host with a few containers, about 1 GB with 40 containers.

### Medium

**M1 — Any local user can plant "containers".**
`internal/collector/containers.go:157-176` falls back to cgroups mode when no runtime socket accepts
a connection; `:432` scans `user.slice/user-*.slice/user@*.service/*.slice/*.service/libpod-payload-*`;
`:455-471` takes the directory name as the ID, cut to 12 bytes; `:681` sets the name to it. Nothing
caps the count (`codec.go:584-589` clamps only at 65,535).

Cgroups mode is what a host without Docker or Podman gets, and what the packaged service gets on a
Docker host: the unit runs as `User=kula` with no supplementary group, so the socket probe fails.
systemd delegates `user@UID.service` to every logged-in user, so that user can `mkdir` matching
directories in their own subtree.

- **Retention.** Each planted directory adds 66 bytes to every raw record. The default 250 MB raw
  tier normally holds 10.8 days; with 1,000 it holds 63 minutes, with 5,000 about 13 minutes, with
  10,000 about 6 minutes. The aggregated tiers carry the same entries. A user can erase the record
  of their own activity and keep it erased.
- **Rollups and collection.** With names changing every second, tier-2 rollups exceed the 64 MiB
  record limit and are never written, and the slowest `WriteSample` took 7.6 s, so ticks are lost.
- **Chosen bytes.** The 12-byte ID and name may hold ESC, HTML and quotes; the byte cut can leave
  invalid UTF-8; two names sharing a prefix give two containers with the same identity.

*Reproduced* by two auditors independently (same 66-byte figure): on the real kernel a directory
created as uid 1001 was reported as a container with cgroups mode forced, and the scale and record
sizes come from package tests. *First-hand:* the fallback and the glob by reading; the delegated
subtree here is owned by the user with `cgroup.max.descendants` = `max`. It was not run end to end
against a live `kula serve`. The dashboard renders such names as text (frontend auditor, live).

*Fix:* accept only 64-hex runtime IDs in both modes; scan the `user.slice` pattern only for Kula's
own uid, or drop it; cap containers per sample; in storage, cap each dynamic collection per record.

> ✅ **Status: fixed** (`6141316`, `037d58a`). Only full 64-hex IDs count, in both modes, and in
> cgroups mode only cgroups owned by root or by Kula's own user; at most 1,024 containers per sample;
> a list past 65,535 entries keeps its first entries instead of garbling the record.

**M2 — The sandbox does not confine Unix sockets, UDP or signals.**
`internal/sandbox/sandbox.go:230` requests `landlock.V5`; the vendored go-landlock v0.11.1 knows
ABI 11.

*First-hand*, on a kernel with Landlock ABI 10, after Kula's own `sandbox.Enforce`:

| Action | Kula's rules | Same grants under ABI 11 |
|---|---|---|
| read `/etc/passwd`, write outside storage, exec, TCP to an ungranted port | denied | denied |
| connect to a Unix socket outside every grant | **allowed** | denied |
| connect to the D-Bus system bus socket | **allowed** | denied |
| send UDP to an arbitrary port | **allowed** | denied |
| signal a process outside the sandbox | **allowed** | denied |

For a root Kula, `docker.sock`, the system bus and `/run/systemd/private` are root-equivalent, so
code execution in the process is not contained; UDP is an exfiltration channel. The packaged unit
runs as `kula` with empty capability sets, which limits this; the snap runs as root.
`docs/dev/09-sandbox.md` notes that socket grants only "document intent", but its opening claim
holds for files and TCP only. Two related gaps: `kula tui` applies no sandbox at all
(`cmd/kula/main.go:300-331`), and the store is opened before the sandbox (`main.go:190` vs `:208`).

*Fix:* request the newest ABI best-effort; add `WithResolveUnix()` to the socket grants; grant UDP
only for DNS when a target is a name; re-test every collector under it.

> ✅ **Status: fixed** (`565547e`, `74f77d2`, `66f7de5`). Kula requests `landlock.V11` best-effort:
> signals and abstract sockets are confined from ABI 6, Unix socket connects outside the grants from
> ABI 9, UDP except DNS (only when a target needs it) from ABI 10. `TestServeUnderTheSandbox` re-runs
> every collector under it. Storage opens after the sandbox, and `kula tui` has a sandbox of its own.
> Landlock does not mediate MPTCP, SCTP or ICMP echo sockets; see §8.5.

**M3 — A fake database server crashes Kula and receives the monitoring password.**
`internal/collector/poller.go:54` calls the poll with no `recover`. lib/pq v1.12.3 and
go-sql-driver/mysql v1.10.1 both index server packets without length checks, lib/pq answers a
cleartext-password request, and the MySQL driver encrypts the password to a key the server
supplies. Both collectors default to TCP on loopback, PostgreSQL with `sslmode: disable`; Kula has
no MySQL TLS option.

Whoever can answer on the configured port can do this: a local user binding 5432 or 3306 while the
database is down or not yet started, an on-path attacker for a remote database, or a compromised
server. One malformed reply panics the driver in the poller goroutine and the whole process exits;
it restarts after 5 s and is crashed again on its first poll.

*Reproduced* against the real `kula serve` under Landlock: both crashes (stack traces in lib/pq
`readBuf.int32` and mysql `packets.go:209`) and both passwords recovered. *First-hand:* the missing
`recover`, by reading. This breaks the AGENTS.md rule that a collector returns nil on failure.

*Fix:* recover around each poll and release the session; recommend Unix sockets for local databases
and `verify-full` over TCP; add a MySQL `tls` option.

> ✅ **Status: fixed** (`0366c14`, `fb70844`, `645d6c3`, `cedee2b`). A panic in a poll fails that poll
> only and leaves no connection or timer behind; MySQL takes `sslmode`/`sslrootcert` and verifies
> the server; the docs say to use the Unix socket locally and `verify-full` across a network. Without
> TLS, whatever answers on the TCP port still receives the password: that is the protocols, now
> documented. Known residue: the MySQL driver leaks one goroutine (about 13 KB) per recovered panic
> during connect, at most 288 a day.

**M4 — A root `kula tui` opens paths chosen by uid 1000.**
`containers.go:86-104` adds `/run/user/1000/podman/podman.sock` to auto-detection when Kula is
root; `:399-414` takes the daemon's `Id` unvalidated; `:708` joins it into a cgroup path; `:733`,
`:809`, `:849-855` read the files there without a bound. The TUI is unsandboxed and
`docs/user/06-tui.md:85` recommends running it with `sudo`.

A fake API returning `"Id":"../../../../home/u/x"` makes root open five files the user placed: a
symlink to `/dev/zero` (unbounded read), a FIFO (the collector hangs), or a device node. Nothing
read is displayed. Under `kula serve` Landlock blocks it.

*Reproduced* as an unprivileged user through a configured `socket_path`, the same code path: the
real `kula tui` opened the attacker's FIFO, and the `/dev/zero` read reached 1.5 GB of heap in a
second. That root auto-selects uid 1000's socket is by reading.

*Fix:* reject daemon IDs that are not 64 hex; read cgroup files bounded, with
`O_NOFOLLOW|O_NONBLOCK`; drop `/run/user/1000` when euid is 0; sandbox the TUI.

> ✅ **Status: fixed** (`97fc8b1`, `6141316`, `66f7de5`, `4997351`). A root Kula no longer picks up
> uid 1000's Podman socket; daemon IDs must be 64 hex; cgroup files are read bounded, with
> `O_NOFOLLOW|O_NONBLOCK`, and only when regular; the TUI runs under its own Landlock sandbox
> (storage read-only, no writes), and the docs suggest running it as the `kula` user.

**M5 — `verify-lock.js` accepts a lockfile that installs something else.**
`addons/chartjs/verify-lock.js:48` only checks that `resolved` starts with
`https://registry.npmjs.org/<name>/-/`; `:53` checks the age of the lockfile's `version` field,
not of what `resolved` fetches; nothing compares `resolved` or `integrity` with the registry.

Accepted, offline with a stubbed registry: `…/@esbuild/linux-x64/-/../../../evil-…/-/evil-…-1.0.0.tgz`,
the same with `%2e%2e`, and a version field that differs from the tarball (which defeats the 14-day
window). Git, `file:`, http, sha1 and wrong-name entries are correctly refused.
`addons/build-chartjs.sh:120` then executes `node_modules/.bin/esbuild` on the machine that builds
every release, and a swapped `chart.js` lands in the bundle every dashboard loads. No CI job runs
the script.

*Reproduced* for the script's acceptance; *first-hand* for the prefix check, by reading. That npm
normalises the URL and fetches the other package is from reading npm's arborist; `npm ci` was not
run.

*Fix:* require `versions[version].dist.tarball === resolved` and `.dist.integrity === integrity`
from the packument already fetched; refuse a `resolved` that `new URL()` would change; run it in CI.

> ✅ **Status: fixed** (`8476809`, `ea0e91b`). Each locked package's `resolved` and sha512 `integrity`
> must equal the registry's `dist.tarball` and `dist.integrity` for its name and version, and the
> 14-day window applies to that release. CI rebuilds the bundle from the lockfile, which runs the
> check, and fails when the committed bundle differs.

**M6 — The `.deb` and the snap leave the config world-readable.**
`addons/build_deb.sh:69-70` creates `/etc/kula` and `/var/lib/kula` with plain `mkdir -p`;
`:120-127` copies the 0644 example to `config.yaml` and runs `chown -R kula:kula /etc/kula`;
`snap/local/launch:20-22` does the equivalent.

That file holds the Argon2id hash and salt, the plaintext Prometheus token and the database
passwords, readable by every local account. The `chown` also lets the daemon's uid rewrite its own
config; systemd's `ReadOnlyPaths` and Landlock hold that back, OpenRC and runit do not. RPM, AUR
and Ansible installs are not affected.

*Reproduced* on `dist/kula-0.21.0-amd64.deb` (`drwxr-xr-x ./etc/kula/`, config `-rw-r--r--`); the
script is unchanged since. The snap is by reading. Contradicts `docs/user/04-configuration.md:12`
and `docs/dev/08-security.md:165`.

*Fix:* `/etc/kula` as `root:kula 0750`; seed the config with `install -m 0640 -o root -g kula`;
leave `/etc/kula` out of the `chown`; `install -m 0600` in the snap launcher; `/var/lib/kula` 0750.

> ✅ **Status: fixed** (`bde67d2`, `922178c`). The `.deb` makes `/etc/kula` 0750 and seeds the config
> `root:kula 0640`, `/var/lib/kula` 0750, with no recursive `chown` of `/etc/kula`; the snap seeds it
> 0600. An upgrade tightens an existing config the same way, unless it is a link or has several
> links, which it leaves alone with a message.

**M7 — Release tarballs carry the builder's uid.**
`addons/release.sh:42` runs `tar -czf` with no owner options; `addons/install_v2.sh:447` and
`addons/install.sh:363` extract as root into `/opt`.

*First-hand:* `dist/kula-0.21.0-amd64.tar.gz` lists every member as `1000/1000`. *Reproduced:* a
simulated root extraction leaves `/opt/kula` and the binary owned by uid 1000. The installer
reaches this path on a host with an unrecognised distribution, no systemd and no Docker. Whoever
holds uid 1000 there can replace the binary that root next starts.

*Fix:* `tar --owner=0 --group=0 --numeric-owner` in `release.sh`; `--no-same-owner
--no-same-permissions` in the installers.

> ✅ **Status: fixed for new releases** (`52059b0`, `ffc0d3e`). Release tarballs carry root as owner,
> and `install_v3.sh` extracts without the archive's owners. The published 0.21.0 tarballs, and
> `install.sh`/`install_v2.sh` (immutable, their checksums are pinned in copies of the
> instructions), are unchanged; those installers extracting a new tarball get root-owned files.

### Low

**L1 — One string over 255 bytes stops all history recording.** `internal/storage/codec.go:188-195`
fails the whole record; `cmd/kula/main.go:265-266` only logs it. A container name, mount path, disk
ID (a 254-byte wwid becomes 759 bytes), GPU or PSU name, or a long `global.hostname` does it.
*Reproduced:* one 300-byte container name failed 8 of 8 ticks and `/api/history` returned no
samples, while the dashboard kept showing live data. It can happen by accident (long CSI mount
paths). *Fix:* drop or truncate the offending element and log once.

> ✅ **Status: fixed** (`42a278d`, `037d58a`): names are cut to 255 bytes on a character boundary, and a
> list keeps its first 65,535 entries.

**L2 — Crafted tier files hang or exhaust `serve`, `inspect`, the TUI and `inspect_tier.py`.**
Needs someone who can hand the operator a file to restore or inspect. *Reproduced:*
(a) `tier.go:828-879` — a repeated 5-byte pattern makes resync checksum at every offset: 8 MiB took
90 s in `kula inspect --verbose`, 2 MiB took 323 s in Python; (b) `store.go:411-429` — a raw record
with a 365-day duration made startup allocate 8.9 GB and overwrite tier 1; (c) `tier.go:184-216`,
`:373` — an unvalidated header offset overflows the wrap check, the file becomes a 9.2 EB sparse
file and the next scan never returns while holding the tier lock. *Fix:* cap checksum candidates;
clamp raw widths in `rollupParts`; validate header offsets and rewrite the wrap check.

> ✅ **Status: fixed** (`0663796`, `41cf88a`, `ef71d68`, `7e051cd`, `70eb919`). Known residue: a tier
> file handed over already sparse can still make the first scan very slow.

**L3 — Startup fails open in three ways.** *First-hand.* (a) Unknown config keys are ignored
without a word (`config.go:537`): `web.auth.enable: true` starts with auth off. (b) An explicit
`-config` that does not exist is seeded from the example and served, auth off
(`cmd/kula/main.go:101-107`). (c) A sandbox rule that cannot be applied disables the whole sandbox
with a warning (`main.go:208-210`): a PostgreSQL `host` pointing at a file with `port: 0` did it.
*Fix:* reject or loudly warn on unknown keys; refuse a missing explicit config; treat a rule error
on a Landlock-capable kernel as fatal.

> **Status: in part.** (a) Fixed (`3cb2e60`): an unknown setting stops startup. (b) In part
> (`20167ac`): an explicit `-config` that cannot be created or read stops startup instead of running
> on built-in defaults; one that does not exist is still created from the packaged example, as on a
> first run. (c) Changed, not fatal, by the maintainer's decision (`737d8ba`, then `7368d6d`): a grant
> that cannot be applied is logged with its setting and left out, and the rest of the sandbox is
> enforced; a kernel that refuses the ruleset (a snap or container syscall filter) leaves Kula
> running unconfined with a warning. The sandbox is a hardening layer, not a requirement.

**L4 — The AI chat proxy trusts the client and the backend** (needs `ollama.enabled`).
*First-hand*, `internal/web/ollama.go`:
(a) `:283-312` — only `prompt` is clamped; `messages[]` is forwarded verbatim with `system` and
`tool` roles at 9 KB each, `context` goes into the system prompt, and `model` accepts any name.
With auth off this is an open LLM proxy at 10 requests a minute per address.
(b) `:383-410` — tool calls per round are uncapped: one request ran 10,000 history queries.
(c) `:651-654` — a negative tool-call index from an OpenAI-compatible backend panics the handler
(recovered; the server stays up).
(d) `:686`, `:361` — backend errors, with the loopback URL and port, go to the client verbatim.
*Fix:* accept only `user`/`assistant` roles and clamp each; restrict `model` to the fetched list;
cap tool calls per round and concurrent chats; range-check the index; return a generic error.

> ✅ **Status: fixed** (`6d53678`, `c81e3ad`, `1d34c08`, `49e6494`, `7305fe6`, `9898c5d`, `3a0d367`,
> `d9f1b46`): only clamped `user`/`assistant` turns on a listed model; 8 tool calls per round; 4
> streaming chats server-wide; the index is range-checked; the client gets a generic error; the proxy
> connects only to loopback addresses. Known residue: the chat does not enforce turn order, and the
> panel's error text is English-only.

**L5 — The AI panel re-renders the whole answer on every chunk** (needs `ollama.enabled`).
`static/js/app/ollama.js:706-711`. *Reproduced:* 400 KB in 8-byte chunks costs 84 s of rendering;
the server cap is 10 MB. A hostile backend, or a local user holding the loopback port while Ollama
is down, freezes the operator's tab. *Fix:* render once per frame; cap the displayed answer.

> ✅ **Status: fixed** (`c1e4202`, `8258369`): one repaint per frame, the answer cut at 32 KiB with a
> notice.

**L6 — One module loads without an integrity check.** `static/index.html:19-56` has 38
`modulepreload` tags for 39 modules; `js/app/disk-identity.js` (since 0.20.0) is missing.
*Reproduced:* a tampered copy boots with no error, while the same change to `state.js` is blocked.
*First-hand:* the count. No test covers it. *Fix:* add the tag and a test that every module has one.

> ✅ **Status: fixed** (`16ed13e`, `da0ff36`): `TestPagesLoadEveryScriptWithIntegrity` walks each
> page's import graph and fails on a module without the right hash.

**L7 — `/metrics` can be broken or inflated through container labels** (needs
`prometheus_metrics.enabled`; follows from M1). `internal/web/prometheus.go:733-738` never
validates UTF-8, and `:464-490` emits 8 series per container with no cap or rate limit.
*Reproduced:* an invalid-UTF-8 label in the output; 10,000 containers give an 18.5 MB scrape. That
Prometheus rejects the whole scrape for the bad label is by reading. *Fix:* `strings.ToValidUTF8`
in `escapeLabel`; a series ceiling.

> ✅ **Status: fixed** (`3a776bd`, `6141316`): labels are valid UTF-8, each family is written once,
> and the 1,024-container cap bounds the series.

**L8 — The TUI writes host strings raw to the terminal, and runs unsandboxed.**
`internal/tui/view.go:116`, `:479-481`, `:492`, `:583`, `:667`, `:743`, `:758-768`, `:895-905`;
collector log lines reach the same terminal. *Reproduced* at the renderer: OSC 52 and title
sequences in the emitted bytes. Reachability is narrow (a mounted volume label, an interface name).
*Fix:* one sanitising helper before styling; point `log` away from the terminal.

> ✅ **Status: fixed** (`fb77ebe`, `8ff58df`, `27c7d86`, `66f7de5`): host strings are escaped before
> styling; log lines wait until the TUI exits and are printed cleaned; the TUI is sandboxed.

**L9 — `kula-scan` against a hostile or shared target.** *Reproduced:*
(a) `cmd/kula-scan/report.go:104-108` prints target bytes raw; a certificate SAN or a status line
carries escape sequences into the operator's terminal and can rewrite earlier result lines
(`-json` is safe).
(b) `scanner.go:384`, `:409` read response headers unbounded: a 512 MiB header line grew the
scanner past 1 GB.
(c) `checks.go:288` — the default run spends two of the five login attempts for the real account,
so the third run within five minutes locks it out, against the docs' "idempotent".
*Fix:* escape control characters; bound the reader; move that check under `-aggressive`.

> ✅ **Status: fixed** (`85452b6`, `e53b7df`, `85f17ec`, `aa827a2`): the report escapes control
> characters; headers and messages are bounded; a default run spends one login and logs out; the
> password can come from `KULA_SCAN_PASSWORD`.

**L10 — `/api/system-info` rereads SMART sensors and batteries on request.**
`internal/sysinfo/sysinfo.go:192-198`, `hardware.go:294-343`. Unauthenticated, at most every 5 s,
bypassing the collector's intervals, for values the default response omits. *Reproduced:* 0.1–0.16 s
per uncached request. *Fix:* skip sensors and power when details are off.

> ✅ **Status: fixed** (`e36e6b7`): read at the collector's intervals, and not at all without details.

**L11 — The custom-metrics socket has no connection limit** (custom metrics on; client in group
`kula`). `internal/collector/custom.go:152-186`. *Reproduced:* 3,000 idle connections hold 129 MiB;
at the descriptor limit the accept loop spins, 792,457 log lines in a second. *Fix:* back off on
accept errors; cap connections.

> ✅ **Status: fixed** (`49796ee`): at most 64 connections, backoff on failed accepts, logged text
> escaped.

**L12 — Hostile monitored services are otherwise unbounded.** *Reproduced:* a container daemon
returning 200,001 entries and a 32 MiB name (`containers.go:300`, `:330-336`, `:406-408`); an Apache
status page whose scoreboard is concatenated quadratically, 537 MiB allocated per poll
(`apache2.go:97-110`). *Fix:* cap count and name length; use a builder.

> ✅ **Status: fixed** (`6141316`, `42a278d`, `16ac3c3`): at most 1,024 containers, names bounded to
> 255 bytes, the scoreboard counted in one pass.

**L13 — The runit service never gets its log directory.** `addons/init/runit/kula/log/run:4`.
By reading: `svlogd` dies, the 64 KiB pipe fills, and Kula blocks on a log write; with request
logging on, a loop on `/health` freezes it. *Fix:* create `/var/log/kula` in the run script.

> ✅ **Status: fixed** (`e900095`).

**L14 — Release and bundle provenance.** Nothing is signed, the checksum file sits beside the
assets, tags are lightweight, and binaries are built from a working tree with `-buildvcs=false` on
a distribution toolchain (`go1.27.0-X:nodwarf5`), so nobody else can reproduce them. No CI job
checks that the committed Chart.js bundle comes from the lockfile, and `addons/install.sh` (v1)
still ships and verifies nothing. *Fix:* build from a signed tag in CI; sign the checksum file;
rebuild and diff the bundle in CI; make v1 a stub.

> **Status: in part** (`ea0e91b`, `5e6becc`): CI rebuilds the Chart.js bundle and fails on a
> difference, and the release checklist names every place the installer checksum is pinned. Not
> done: signing, builds from a signed tag, reproducible binaries. `install.sh` (v1) stays as
> published, since its checksum is pinned in copies of the instructions; `install_v3.sh` is current.

**L15 — The documented "verify the installer" snippet runs it anyway.** `README.md:129-134`,
`docs/user/02-installation.md:27-33`: `sha256sum -c || rm -f …` is followed by `bash` on the same
`/tmp` name. By reading. *Fix:* `sha256sum -c && bash "$KULA_INSTALL"`.

> ✅ **Status: fixed** (`df92614`, `ffc0d3e`).

**L16 — The reverse-proxy page enables `trust_proxy` without closing the direct listener.**
`docs/user/13-reverse-proxy.md:13-20`, `:59-69`. A client connecting straight to the port picks its
own `X-Forwarded-For`, so a fresh login-limiter bucket per request. By reading. *Fix:* add
`listen: "127.0.0.1"` to the example; consider a `trusted_proxies` list.

> ✅ **Status: fixed** (`0da15b8`): the example keeps Kula's port on loopback. No `trusted_proxies`
> list was added.

### Hardening

> **Status:** all done except an HTTP connection cap, `bash` in the image, the archived
> `yaml.v3`, and tightening an existing storage directory (declined); per item in §8.3.

- **CSP:** no `base-uri` or `form-action` (`server.go:366`); the game-score origin is allowed on
  every page, not only the game. Add `base-uri 'self'; form-action 'self'; object-src 'none'`.
- **HTML pages:** `renderTemplate` sets no `Cache-Control`; the nonce'd pages should be `no-store`.
- **Login form:** the password stays in the hidden field after sign-in (`auth.js:154-163`).
- **SSE framing:** `ollama.go:507`, `:637` escape only `\n`. A `\r` reaches the wire (first-hand);
  the shipped dashboard splits on `\n` only and is unaffected. JSON-encode each payload.
- **Storage:** six section counts are cast to uint16 unclamped (`codec.go:464`…`:752`); the decoder
  pre-allocates from untrusted counts (`codec.go:995`…); tier, migration and backup files are
  opened without `O_NOFOLLOW`; an existing storage directory is never tightened;
  `inspect_tier.py:1227` prints magic bytes raw.
- **Web:** the Argon2 slot is released without `defer` (`server.go:1059`); a failed socket `chmod`
  is only logged (`:748`); no cap on concurrent HTTP connections; `ollama.url` trusts the resolver
  for `localhost`.
- **Collectors:** status URLs are logged with any `user:pass@` (`collector.go:144`, `:149`);
  remote-chosen text reaches logs unescaped; a configured container socket that is down falls back
  to auto-detection silently; byte cuts split UTF-8.
- **Tools:** `kula-scan` takes the password only on the command line; `/metrics` repeats
  `# HELP`/`# TYPE` per series, which `promtool` rejects; `gen-mock-data` treats an explicit
  `-config` as optional; the TUI's read-only open follows symlinks and blocks on a FIFO.
- **Docker:** the runtime user owns the binary and config; the uid is not pinned; no `cap_drop`,
  `no-new-privileges` or `read_only`; no `.dockerignore`.
- **Deployment:** reading container names through the socket means `docker` group membership,
  which is root on the host, and the docs do not say so; the snap daemon runs as root; OpenRC and
  runit add no confinement beyond the user; `gopkg.in/yaml.v3` is archived upstream; `go get -u`
  is recommended with no age window; `SECURITY.md:15` invites public issues for vulnerabilities.

## 4. Documentation claims that do not hold

> ✅ **Status:** all corrected, in the code or the page; see §8.4.

- `docs/dev/08-security.md:127` and `AGENTS.md`: all served JavaScript carries integrity (L6).
- `docs/dev/09-sandbox.md`, opening: a compromised process cannot open arbitrary connections (M2).
- `docs/dev/08-security.md`: prompt clamped to 2000 runes — it does not bound what reaches the
  model (L4).
- `docs/dev/08-security.md:165` and `docs/user/04-configuration.md:12`: storage directory 0750,
  config 0600 — not on the `.deb` or the snap (M6).
- `AGENTS.md`, `docs/dev/04-collector.md`: a collector failure never breaks the sample (M3, L1);
  costly readings run at their own interval (L10).
- `docs/dev/13-kula-scan.md`, `cmd/kula-scan/README.md:111`: non-destructive and idempotent (L9).
- `docs/dev/03-building.md:109-117`, `AGENTS.md:140`: what `verify-lock.js` guarantees, and a
  bundle reproducible "byte for byte" that nothing checks (M5, L14).
- `docs/user/08-application-monitoring.md`: silent on cgroups mode counting user-created cgroups
  (M1); `docs/user/06-tui.md:85` recommends `sudo kula tui` without the consequence (M4).
- `docs/dev/08-security.md:145`: `filepath.Abs` as directory-traversal resistance protects nothing
  an attacker controls.
- `docs/dev/15-packaging.md:64` names `alpine:3.24.1` (the image uses 3.24.3) and `:100` still
  points at `install.sh` v1.

## 5. Checked and found sound

**Web core (first-hand).**
- Host check: 19 Host variants refused or answered as documented; `/health` stays open; a foreign
  Host cannot upgrade to WebSocket.
- Routes: with auth on, every `/api/*` except login, logout and auth status, and `/ws`, answer 401
  without a session.
- Sessions: 256-bit tokens, hashed at rest, new per login, `HttpOnly; SameSite=Strict`, absolute
  lifetime enforced; logout revokes cookie and bearer.
- CSRF and WebSocket: foreign, `null` and missing Origin refused for POST; foreign and `null`
  refused for WebSocket with and without a valid cookie; sessions re-checked every 30 s.
- Login: limiter before body parse, one Argon2id per attempt for unknown names, bounded hashing;
  auth enabled with no credentials fails closed.
- Headers on HTML, JSON, static, 404 and redirects; a fresh 128-bit nonce per response.
- The sandbox denies file access outside its grants, exec, and TCP to ungranted ports.
- `kula-scan` default run: 20 pass, 0 fail on the open instance; 30 pass, 0 fail with auth.

**Frontend (auditor, live in Chromium).** Hostile container names, model names, AI answers,
hostname and custom-metric names: nothing executed, no CSP violation, no off-origin request. The AI
renderer emits only fixed tags, no links or images. All 40 first-party scripts parse to identical
syntax trees before and after minification. No same-origin endpoint serves attacker bytes as
script. The CSRF token never reaches a URL. 26 locales, 498 keys, no markup.

**Collectors (auditor).** A 90-second fuzz of the `/proc` and `/sys` parsers found no panic; no
data race at a 20 ms interval; nginx and Apache2 clients do not follow redirects, cap bodies and
verify TLS; database DSN values are quoted and queries parameterised; the custom socket has no
permission window and stores only configured names; no command is executed.

**Storage (auditor).** The seek sidecar is adopted only on an exact match and every point is
re-confirmed; no length prefix allocates beyond the record cap; a 10-minute fuzz of the tier-open
path found only L2(c); backups are 0600, pruned by parsed name only, and confined by Landlock;
there is no restore or decompression code to attack; time arithmetic is safe for years 0000–9999.

**Prometheus, TUI, scanner (auditor).** Metric and label names are constants and label values are
escaped, so no series can be forged; the bearer comparison is constant-time and the token is never
logged; twelve path variants never served `/metrics` to a foreign Host; `show_system_details` is
enforced server-side; the TUI has no key that writes files or runs commands; `kula-scan` never
follows redirects, verifies TLS by default and sends credentials only to the target's login.

**Supply chain (auditor).** `install_v2.sh` requires a checksum and extracts as the user; the
systemd unit is well hardened; workflows use pinned `actions/*` only, a read-only token and no
untrusted input in `run:`; `go mod verify` passes with no `replace`; Docker bases are pinned by
digest; RPM, AUR and Ansible set correct modes.

## 6. Not covered

- M1 end to end against a live `kula serve`; M4 and the uid-1000 socket selection as real root;
  `npm ci` on the crafted lockfiles (M5); a real Prometheus server for L7.
- udisks, autofs and NetworkManager routes to hostile mount and interface names; real-terminal
  handling of the escape sequences in L8 and L9.
- `prometheus.go` and `minify.go` were read by the auditors, not the lead; the CSS minifier only
  by reading; five frontend modules scanned for sinks rather than read line by line.
- Browsers other than Chromium; `base_path`, `security.headers: false` and `allowed_origins` live.
- `kula-scan -aggressive` and `-fuzz`; behaviour behind a TLS-terminating proxy.
- Published release assets, images, the snap and the AUR package against `dist/`; action SHAs
  against tags; `addons/ci-local/run-workflow.py`.
- Driver fuzzing beyond the two crafted packets in M3; legacy v1 JSON migration beyond fuzz seeds.
- Which findings are regressions in 0.22.0.

## 7. Suggested order of work

> **Status:** done, except what §8.5 lists as open.

1. **Before building release artifacts:** M6 and M7 (they ship in the packages), L13, L15.
2. **H1:** the handler semaphore is a small change; the byte budgets can follow.
3. **One pass over what the host can put into a sample:** M1, L1, L12 and the container half of M4
   share a fix — validate IDs, cap counts and lengths, never fail a whole record for one element.
4. **M3:** `recover` in the poller.
5. **M5** and the CI checks in L14.
6. **M2**, with L3(c) and the TUI: a larger change that needs every collector re-tested under the
   new rules.
7. The remaining Low items, then the documentation in §4.

## 8. Remediation

The fixes are the commits from `bde67d2` to `d74ddf0`, all released in 0.22.0. At `d74ddf0`, `./addons/check.sh` passes: govulncheck
finds no vulnerabilities in called code, gofmt and vet are clean, all 12 packages pass under
`-race`, and golangci-lint reports 0 issues. The five browser suites (dashboard, performance, live
streaming, session expiry, AI panel) passed at `4997351`. The two later commits change no
frontend code.

### 8.1 Findings

| ID | Status | Commits |
|---|---|---|
| H1 | ✅ Fixed | `3f8c8f4` `41392b2` `5a50a96` `c12cb30` `d74ddf0` |
| M1 | ✅ Fixed | `6141316` `037d58a` |
| M2 | ✅ Fixed; MPTCP, SCTP, ICMP open (§8.5) | `565547e` `74f77d2` `66f7de5` |
| M3 | ✅ Fixed | `0366c14` `fb70844` `645d6c3` `cedee2b` `e4cb9b7` |
| M4 | ✅ Fixed | `97fc8b1` `6141316` `66f7de5` `4997351` |
| M5 | ✅ Fixed | `8476809` `ea0e91b` |
| M6 | ✅ Fixed | `bde67d2` `922178c` |
| M7 | ✅ Fixed for new releases | `52059b0` `ffc0d3e` |
| L1 | ✅ Fixed | `42a278d` `037d58a` |
| L2 | ✅ Fixed | `0663796` `41cf88a` `ef71d68` `7e051cd` `70eb919` `92948e9` |
| L3 | (a) fixed; (b) in part; (c) changed by decision | `3cb2e60` `20167ac` `737d8ba` `7368d6d` |
| L4 | ✅ Fixed | `6d53678` `c81e3ad` `1d34c08` `49e6494` `7305fe6` `9898c5d` `3a0d367` `d9f1b46` `00e0487` |
| L5 | ✅ Fixed | `c1e4202` `8258369` |
| L6 | ✅ Fixed | `16ed13e` `da0ff36` |
| L7 | ✅ Fixed | `3a776bd` `6141316` |
| L8 | ✅ Fixed | `fb77ebe` `8ff58df` `27c7d86` `4822ecd` `66f7de5` |
| L9 | ✅ Fixed | `85452b6` `e53b7df` `85f17ec` `aa827a2` `ee5b19d` |
| L10 | ✅ Fixed | `e36e6b7` |
| L11 | ✅ Fixed | `49796ee` |
| L12 | ✅ Fixed | `6141316` `42a278d` `16ac3c3` |
| L13 | ✅ Fixed | `e900095` |
| L14 | In part: bundle rebuilt in CI; no signing or reproducible builds | `ea0e91b` `5e6becc` |
| L15 | ✅ Fixed | `df92614` `ffc0d3e` |
| L16 | ✅ Fixed (no `trusted_proxies` list) | `0da15b8` |

### 8.2 Maintainer decisions

- **The sandbox never stops Kula (L3(c)).** A grant the config asks for that cannot be applied is
  logged with its setting and left out, and the rest is enforced. A kernel that refuses the ruleset
  leaves Kula unconfined with a warning, as a kernel without Landlock always did (`7368d6d`).
  Before the audit, any such error dropped the whole sandbox.
- **Memory:** `GOMEMLIMIT=512MiB` under `MemoryMax=1G`. On a host with about 40 containers a flood of
  history requests needs about 1 GB, above the soft limit: the garbage collector runs harder there
  (its CPU share is capped), and nothing fails.
- **Old modes are not tightened on upgrade:** tier files created before 0.4.0 stay 0644, and a
  storage directory created before 0.8.3 stays 0755. The docs say so (`9ee67dd`).
- **seccomp:** evaluated in §8.6, not implemented.

### 8.3 Hardening items

| Item | Status |
|---|---|
| CSP `base-uri`, `form-action`, `object-src`; the score origin only on the game page | ✅ Fixed `1289d6b` |
| HTML pages `no-store` | ✅ Fixed `5e28495` |
| Password left in the login form | ✅ Fixed `c12d648` |
| SSE framing | ✅ Fixed `dcc3028` |
| Storage: section counts cast to `uint16` unclamped | ✅ Fixed `037d58a` |
| Storage: decoder pre-allocates from untrusted counts | ✅ Fixed `70eb919` |
| Storage: tier, migration and backup files opened through links | ✅ Fixed `6e1f4c8` `92948e9` `a3484ae`; also `sessions.json` `756306d`, `nvidia.log` `14247a9` |
| Storage: an existing storage directory is never tightened | Declined (§8.2), documented `9ee67dd` |
| `inspect_tier.py` prints magic bytes raw | ✅ Fixed `4561c95` |
| Web: Argon2 slot released without `defer` | ✅ Fixed `662a112` |
| Web: a failed socket `chmod` only logged | ✅ Fixed `0530292` |
| Web: no cap on concurrent HTTP connections | **Open**; history (2), AI streams (4) and the custom socket (64) are capped |
| Web: `ollama.url` trusts the resolver for `localhost` | ✅ Fixed `3a0d367` |
| Collectors: status URLs logged with credentials; remote text unescaped in logs | ✅ Fixed `9d0698d` `49796ee` |
| Collectors: a configured container socket that is down falls back silently | ✅ Fixed `8ddbc6e` |
| Collectors: byte cuts split UTF-8 | ✅ Fixed `42a278d` `9d0698d` |
| `kula-scan` password only on the command line | ✅ Fixed `aa827a2` |
| `/metrics` repeats `# HELP`/`# TYPE` | ✅ Fixed `3a776bd` |
| `gen-mock-data` treats an explicit `-config` as optional | ✅ Fixed `5409ad7` |
| TUI read-only open follows symlinks, blocks on a FIFO | ✅ Fixed `92948e9` |
| Docker: owner, uid, `cap_drop`, `no-new-privileges`, `read_only`, `.dockerignore` | ✅ Fixed `249d68f` `fba9691`; the image still ships `bash` |
| Deployment docs: `docker` group is root, snap runs as root, OpenRC/runit confinement | ✅ Documented `6c2d1bc` |
| `gopkg.in/yaml.v3` archived upstream | **Open** |
| `go get -u` with no age window | ✅ Fixed `af5fb33` |
| `SECURITY.md` invites public issues | ✅ Fixed `60376c2` |

### 8.4 Documentation claims (§4)

| Claim | Status |
|---|---|
| All served JavaScript carries integrity | ✅ True now (L6) |
| `09-sandbox.md`: no arbitrary connections | ✅ Rewritten for ABI 10; a "Not confined" section lists MPTCP, SCTP, ICMP and netlink (`565547e`) |
| Prompt clamped to 2000 runes | ✅ The page states every limit the proxy enforces (`d9f1b46`) |
| Storage 0750, config 0600 | ✅ True now on the `.deb` and the snap (M6) |
| A collector failure never breaks the sample; costly readings at their own interval | ✅ True now (M3, L1, L10) |
| `kula-scan` non-destructive and idempotent | ✅ One login per default run (`85f17ec`) |
| `verify-lock.js` guarantees; bundle reproducible byte for byte | ✅ Checked now, in CI (M5, L14) |
| cgroups mode; `sudo kula tui` | ✅ Documented (`6141316`); the TUI page suggests the `kula` user (`4997351`) |
| `filepath.Abs` as traversal resistance | ✅ Corrected (`f71a22f`) |
| `15-packaging.md`: Alpine version, installer | ✅ Corrected (`249d68f`, `ffc0d3e`) |

### 8.5 Still open

- **Network that Landlock does not mediate.** MPTCP reaches any TCP port. Kula's own listener is
  MPTCP, because Go makes listeners MPTCP by default since 1.24. SCTP and ICMP echo sockets are
  also unmediated, and systemd's default `net.ipv4.ping_group_range` admits every group. Outside
  the systemd unit's `RestrictAddressFamilies`, every other address family (VSOCK, ALG, TIPC, …)
  and every netlink protocol is reachable too. On kernels below ABI 10, UDP reaches any address;
  below ABI 4, TCP does. §8.6 covers the first group.
- **Snap:** not tested with the current sandbox. A snap filter that refuses Landlock now only logs
  a warning.
- **L14:** signing, builds from a signed tag, reproducible binaries; `install.sh` v1 stays as
  published.
- **Hardening:** an HTTP connection cap; `bash` in the image; `yaml.v3`.
- **Residues:** the MySQL driver's goroutine per recovered connect panic (M3); a slow first scan of
  a tier file handed over already sparse (L2); chat turn order and English-only panel errors (L4);
  no `trusted_proxies` list (L16).

### 8.6 seccomp filter: assessment

**Viable.** A pure-Go prototype (not in the repository) (`golang.org/x/sys/unix`, already a direct dependency; no cgo)
installs a 29-instruction filter with `SECCOMP_FILTER_FLAG_TSYNC`. On Linux 7.2 that put it on all
13 threads of the Go runtime at once. The filter:

- refuses other architectures and x32 syscalls (this kernel accepts x32) with `ENOSYS`;
- allows `socket()` only for `AF_UNIX`, `AF_NETLINK` with `NETLINK_ROUTE`, and `AF_INET`/`AF_INET6`
  stream sockets with protocol 0 or TCP and datagram sockets with protocol 0 or UDP. It reads only
  the low 32 bits of each argument, as the kernel does;
- refuses `io_uring_*`, which can create sockets out of seccomp's sight.

| Probe | Before | After |
|---|---|---|
| MPTCP, IPv4 and IPv6 | ok | `EPROTONOSUPPORT` |
| SCTP (stream, and seqpacket with protocol 0) | ok | refused |
| ICMP echo | ok | refused |
| VSOCK, ALG | ok | `EAFNOSUPPORT` |
| netlink netfilter | ok | refused |
| x32 `getpid` / `io_uring_setup` | ok / reachable | `ENOSYS` |
| TCP, UDP, Unix, netlink route, `net.Interfaces`, name lookup | ok | ok |
| `net.Listen("tcp")` | MPTCP | plain TCP (Go's own fallback); dial and read work |

**Cost.**

- *Runtime:* about 15–20 ns per syscall here (`getpid` 317 → 332 ns, an 8-byte `read` 447 → 468 ns)
  and about 100 ns per `socket()`. Kula makes about 1,750 syscalls a second, startup included
  (strace, 30 s), which comes to about 35 µs of CPU a second, under 0.01 % of a core.
- *Code:* about 150 lines in `internal/sandbox`: the filter, an install that follows the sandbox's
  warn-and-continue rule, and constants for amd64, arm64 and riscv64 (other architectures skip it
  with a log line). No new dependency. Tests would be about 200–250 lines: a small BPF interpreter
  that checks every verdict for all three architectures on any build host, and a child-process
  probe on the real kernel next to `TestServeUnderTheSandbox`. Plus the sandbox, security and
  troubleshooting pages. About a day of work.
- *Maintenance:* it is an allowlist, like the Landlock grants. A collector that needs another
  socket kind (ICMP ping, SCTP) must extend it, and a new architecture needs its constants. If the
  Go runtime ever adopts io_uring, the filter must allow it, and a test would catch that.
- *Risks:* an environment that refuses `seccomp(2)` gets a warning and no filter. Filters pass to
  child processes, and Kula starts none. Kula's listener would no longer be MPTCP; plain-TCP
  clients are unaffected.

**What it does not do.** seccomp cannot read the address a `connect` or `sendto` passes, so which
hosts and ports are reachable stays Landlock's job. On kernels below ABI 10, UDP stays open.

**Cheap extension.** About 20 more instructions could refuse syscalls Kula never makes that widen
the kernel surface an unprivileged process can reach: user namespaces (`unshare`, `clone` with
`CLONE_NEWUSER`, `clone3`), `userfaultfd`, `keyctl`/`add_key`, `bpf`, `perf_event_open`, `ptrace`,
`process_vm_*` and the mount family. Each needs the same check against the collectors.

**Recommendation:** add it to the sandbox package and apply it right after Landlock in
`kula serve` and `kula tui`. Under the systemd unit it adds MPTCP, SCTP, ICMP, netlink-protocol
narrowing and io_uring to what the unit already restricts. Outside systemd (OpenRC, runit, Docker,
the snap, a manual run, the TUI), it also replaces `RestrictAddressFamilies` and
`SystemCallArchitectures`.
