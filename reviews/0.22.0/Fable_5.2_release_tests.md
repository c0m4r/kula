# Kula 0.22.0 — Final regression tests before release

| | |
|---|---|
| **Commit tested** | `5cb10ea` (`main`, equal to `origin/main`, clean tree) |
| **Compared with** | tag `0.21.0` (281 commits, 268 files back) |
| **Date** | 2026-11-13/14 |
| **Tester** | Claude Fable 5.2 (gates and the verification of every finding marked *Confirmed*), six parallel testers (Claude Opus 5.5) for the hands-on tracks |
| **Status** | No repository file was changed during testing. The findings were fixed afterwards, before the release; each is marked ✅ **Fixed** with its commit. Every fix is in tag `0.22.0`. |

An item marked *Confirmed* was reproduced or read in the code by the lead tester. An item
marked *Reported* is a tester's result that was not re-checked.

## 1. Verdict

Every scripted gate is green, on Go 1.27.1 locally and on Go 1.26.11 in the local CI container.
Stored data, the API, Prometheus, the WebSocket stream, packages and browser state all carry
over from 0.21.0 without a regression.

Three things had to be fixed before the tag. All three were fixed:

1. The Docker multi-arch push would fail: the pinned Go base image has no `linux/riscv64` (§3.1).
   ✅ **Fixed.**
2. MySQL `host: "[::1]"` worked in 0.21.0 and failed in 0.22.0 (§3.2). ✅ **Fixed.**
3. `kula-scan -aggressive` reported a false HIGH failure against an instance without auth
   (§3.3). ✅ **Fixed.**

One older bug was the most visible thing found: an open live dashboard stopped redrawing its
charts after a container or sensor disappeared (§3.4, also in 0.21.0). ✅ **Fixed**, with a
regression test.

### Fix status

| Finding | Status |
|---|---|
| §3.1 Docker push for riscv64 | ✅ **Fixed** `329770d` |
| §3.2 MySQL `[::1]` | ✅ **Fixed** `22af990` |
| §3.3 `kula-scan -aggressive` without auth | ✅ **Fixed** `83778c8` |
| §3.4 Live charts freeze | ✅ **Fixed** `40f80be` |
| §3.5 Upgrade notes and docs | ✅ **Fixed**, except open dashboards across the upgrade (**Open**) |
| §3.6 Low | ✅ **Fixed**, except a negative `session_max_lifetime` (**Not changed**, by design) |
| §2 Extra checks | ✅ **Fixed**, except one shellcheck false positive (no change needed) |

## 2. Gates

| Gate | Result |
|---|---|
| `./addons/check.sh` (govulncheck, gofmt, vet, race tests, golangci-lint) | pass; most packages came from the test cache, so the race suite was re-run below |
| `go test -count=1 -race ./...`, twice, Go 1.27.1 | pass both times, 12 packages, no data race |
| `go test -count=1 -shuffle=on ./...` | pass |
| `./addons/test-frontend-regressions.sh` (5 browser fixtures) | pass |
| `./addons/ci-local.sh run-all`, Go 1.26.11, uncached | pass: `ci` (govulncheck, vet, race 84 s, golangci-lint, runner tests, packaging helpers, build, smoke), `chartjs-bundle` (rebuild equals the committed bundle), `browser` (5 fixtures), `semgrep` (586 rules, 451 files, 0 findings), `zizmor` (no findings, 5 suppressed) |
| `./addons/fuzz.sh 30s` | pass, 18 targets |
| `go vet` and test compilation for `arm64` and `riscv64` | pass |
| `go mod verify`, `verify-lock.test.js`, `check_helpers.sh`, ci-local unit tests | pass |

govulncheck reports nothing that affects the code; one module-level note (GO-2026-5932,
`x/crypto/openpgp`, not imported).

Extra checks that are not part of the gate:

- `go mod tidy -diff` would drop unused `go.sum` lines of older versions (go-landlock 0.11.0,
  terminfo 1.2.0, x/sys 0.49.0, x/text 0.43.0). ✅ **Fixed** `e29bfaa`.
- `mypy --strict` (2.3.1): 6 errors in `addons/inspect_tier.py`, whose 0.21.0 version was
  clean, 2 in `go_modules_updates.py`, the rest in `addons/ci-local` (partly missing `yaml`
  stubs). `docs/dev/12-testing.md` names this check. ✅ **Fixed** `e1feb8a`.
- `black --check` (26.10) would reformat 4 files; it reformats 0.21.0's `inspect_tier.py`
  too, so this is formatter drift. ✅ **Fixed** `a44518c`.
- shellcheck: one false positive (`addons/docker/run.sh:31`, unchanged since 0.21.0; no change
  needed) and an unused `RED` in `test-frontend-regressions.sh` (✅ **Fixed** `d49c8ac`).

## 3. Findings

### 3.1 Docker push fails for riscv64 — release step blocker. *Confirmed.* ✅ **Fixed** `329770d`.

`addons/docker/Dockerfile:1` pinned `golang:1.26.11-trixie@sha256:fa340cb5…`. That index has
386, amd64, arm64, arm/v7, ppc64le and s390x, and no riscv64; the 0.21.0 pin
(`1.26.8-trixie@771f…`) had it. `push.sh` and `push_ghcr.sh` build
`linux/amd64,linux/arm64,linux/riscv64`, and the builder stage had no `--platform`, so
BuildKit needed a riscv64 Go image. Introduced by `bbeb8b7`. No workflow builds the image.

Tester's prototype, verified to cross-compile the riscv64 stage:
`FROM --platform=$BUILDPLATFORM golang:1.26.11-trixie@sha256:… AS builder`.

### 3.2 MySQL `host: "[::1]"` no longer connects — regression. *Confirmed in code; reproduced three times by the tester.* ✅ **Fixed** `22af990`.

`internal/collector/mysql.go:58` built the address with `net.JoinHostPort`, which brackets a
host that is already bracketed: `[[::1]]:3306`. 0.21.0 used `fmt.Sprintf("tcp(%s:%d)")`, so the
bracketed form was the only IPv6 form that worked there. Log:
`[mysql] cannot connect to "[[::1]]:28420": … missing port in address`. The sandbox also
treated the value as a host name and added a DNS grant. The Upgrade notes did not mention it.

Suggested fix: strip one pair of brackets, best when the config is loaded, so the sandbox grant
and the log label are right too. PostgreSQL rejected `[::1]` in both versions, as libpq does;
the fix makes PostgreSQL accept it as well.

### 3.3 `kula-scan -aggressive` fails falsely without auth — new defect. *Confirmed.* ✅ **Fixed** `83778c8`.

0.22.0 answers `/api/login` with 404 while auth is off. `runOversizedBodyCheck` and
`runLoginRateCheck` (`cmd/kula-scan/checks_aggressive.go`) did not skip in that case:

```
WARN  INPUT-AGG  Login body size cap [MEDIUM]
FAIL  RATE-LOGIN Login brute-force rate limiting [HIGH]
FAILING safeguards at/above HIGH: RATE-LOGIN        (exit 1)
```

The default run passes (20 pass, 0 fail, 2 warn). Suggested fix: return a SKIP when
`!s.authEnabled`, as BYPASS-XFF already does.

### 3.4 Live charts freeze after a container or sensor disappears — pre-existing. *Confirmed on HEAD and on 0.21.0.* ✅ **Fixed** `40f80be`.

Reproduction: `gen-mock-data -duration 8m` into a fresh store, `kula serve` on it, open
`/?range=300` at 1440×900. On HEAD the first error came after 98.7 s, on 0.21.0 after 104.4 s:

```
Live sample error: TypeError: Cannot read properties of undefined (reading 'data')
    at _onDataPush … appendEnvelopePoint … appendAlignedTick
```

It then repeated every second. `pruneDeadContainers` (`container-apps.js:292`) and
`ensureSensorDatasets` (`chart-envelope.js:97`) replaced `chart.data.datasets`; an off-screen
chart defers `update()`, so its Chart.js controllers kept stale indices (7 controllers for 6
datasets on the container charts). The next push threw inside `addSampleToCharts`, and
`pushLiveSample` skipped `trimChartsToTimeRange` and `updateAllCharts`: gauges and the clock
stayed live, no chart redrew. Scrolling the stale chart into view, or changing the range,
recovered it. Real-world trigger: a container stops while a live view is open, and its last
point leaves the window.

Tester's prototype (no error in 480 s): call `chart.update('none')` after the filter in both
places, only when the dataset count changed in `ensureSensorDatasets`. The fix removes datasets
only through `dropDatasets()` (`chart-envelope.js`), which updates the chart at once, and
`TestChartDatasetsAreDroppedThroughTheHelper` now guards it.

### 3.5 Upgrade notes and docs. *Confirmed unless marked.*

- **Packages do not restart the daemon.** ✅ **Fixed** `8d91d59` (packages) and `de2676a`
  (`install_v3.sh`). `build_deb.sh:144-145` and `build_rpm.sh:203-204` ran `systemctl enable`
  and `start`; a running 0.21.0 kept running after the upgrade until it was restarted, and a
  disabled service was enabled again. Same in 0.21.0, but this release's upgrade notes only
  take effect after a restart and did not ask for one.
- **Downgrade note.** *Reported.* ✅ **Fixed** `573f5a9`. 0.21.0 on a 0.22.0 directory does not
  refuse and does not damage anything: it skips the new records silently, so the 0.22.0 period
  reads as a gap (a straddling window even answers `complete:true`). Upgrading again brings the
  data back. The backup that helps is one taken before the upgrade. `docs/user/12-backups.md:58-60`
  said "may misread its records or refuse them".
- **Open dashboards.** *Reported.* **Open.** With auth, a 0.21.0 tab left open across the
  upgrade keeps retrying the WebSocket and shows stale data until it is reloaded.
- `docs/user/06-tui.md:22-23` said a sandbox that cannot be applied stops the TUI; it warns
  and runs unconfined (`cmd/kula/main.go:340`). ✅ **Fixed** `e1c7274`.
- `docs/dev/09-sandbox.md:123-124` said a socket host through a regular file stops startup;
  lines 35-38 of the same page, and the code, leave it out with a warning. ✅ **Fixed** `e1c7274`.
- `docs/user/08-application-monitoring.md:337` said a poll is cut after 10 s; the code adds
  `abortGrace` (5 s), and the tester measured 16 s twice. ✅ **Fixed** `e1c7274`.
- The Docker note about `/run/utmp` or `/run/systemd/sessions` was only in
  `docs/user/02-installation.md`; `README.md:150,156` and `docker-compose.yml` lacked it.
  *Reported.* ✅ **Fixed** `b514d95`.
- Three broken anchors: `docs/dev/09-sandbox.md:195`, `docs/dev/10-frontend.md:405,410`.
  *Reported; present in 0.21.0.* ✅ **Fixed** `e1c7274`, `78c1848`.
- After the tag: `README.md:139-140,163-164,171-172` still carried the 0.21.0 URLs and
  checksums; the CHANGELOG heading said "TBA". ✅ **Fixed** `92f8679`, `5d309ac`.

### 3.6 Low

- **Tests reach the real `~/.kula`.** *Confirmed.* ✅ **Fixed** `e1725c6`. Seven tests in
  `internal/config` and `TestHashPasswordCommand` in `cmd/kula` loaded the default storage
  directory, so they created `~/.kula` and wrote a probe file there, and failed when `HOME` was
  not writable (`HOME=/nonexistent go test ./internal/config ./cmd/kula`). Six of them did so
  in 0.21.0.
- **Sandbox summary line.** *Reported.* ✅ **Fixed** `0109665`. `sandbox.go:595-596` listed the
  storage directory, and `:565` a `web.unix_socket`, even when that grant was just left out.
- **MariaDB 11.8.** *Reported; same in 0.21.0.* ✅ **Fixed** `c7a15c1`.
  `innodb_buffer_pool_hit_pct` read −678.95: `mysql.go:481-484` had no guard for reads above
  requests.
- **Validation.** *Reported.* `allowed_origins` accepted ports 0 and 99999; `allowed_hosts`
  accepted entries that can never match (✅ **Fixed** `325efde`). A negative
  `session_max_lifetime` turns the cap off (pre-existing; **Not changed**: zero or negative is
  the documented opt-out).
- **CLI.** *Reported; pre-existing.* ✅ **Fixed** `287e030`. `kula serve --config X` ignored `X`
  (flags after the command were not parsed; the image's `CMD` worked only because of its
  `WORKDIR`); `serve --help` started the daemon; `inspect` exited 0 when every tier failed to
  open.
- **API.** *Reported; pre-existing.* ✅ **Fixed** `1fc379d`, `d520b79`, `bc19747`, `536bf29`.
  Read-only routes answered any method; `POST /api/logout` sent JSON as `text/plain`; an empty
  history window returned `"samples": null`; the `/metrics` bearer scheme was case-sensitive.
- **Packaging.** *Reported; pre-existing.* ✅ **Fixed** `a50c14f`. `build_aur.sh:149,219` copied
  the helper scripts into `/usr/share/kula/` instead of `/usr/share/kula/scripts/`.
- TCP retransmissions were stored but had no Prometheus family. *Reported.* ✅ **Fixed** `fdb1083`
  (`kula_tcp_retransmissions_per_second`).

## 4. What the testers exercised and found sound

- **Stored data across the upgrade.** A live 0.21.0 directory, a three-day mock with tiers 0
  and 1 wrapped and a small mock with all three wrapped: 65 of 66 history responses identical
  between versions; the one difference is the documented fix for history missing after a
  restart. Mixed old and new tiers, the `tier_N.idx` life cycle (adoption, rejection of stale,
  foreign, truncated and garbage sidecars), SIGKILL recovery, backups taken while writing, a
  0.21.0 backup opened by 0.22.0, `kula inspect` against `inspect_tier.py`, and the TUI on
  upgraded data.
- **Data surface on this host, side by side.** `/api/current` field by field (differences only
  where the CHANGELOG says so: disk utilisation, TCP retransmissions, logged-in users, each
  checked against the kernel's numbers), every route's status, headers and JSON shape, 85
  Prometheus families with identical names, types and labels, a gap-free WebSocket stream for
  10 minutes, footprint (RSS 34–36 MB, about 1.1 % CPU, no growth), clean SIGTERM and restart.
- **Application collectors against real services.** nginx, Apache2, PostgreSQL 17, MySQL 8.4,
  MariaDB 11.8, PgBouncer, Docker and rootless Podman, custom metrics: rates match the
  services' own counters and 0.21.0; no spike after restarts or failed polls; 1 s sampling kept
  with services paused or down (0.21.0 lost 70 % of samples there); TLS modes; the PgBouncer
  upgrade note; redirects; `https://` targets under the sandbox.
- **Operator surface.** 106 variants of `config.example.yaml` from every tag since 0.1.0 load
  and start on 0.22.0; every Upgrade note that stops startup does so with a clear error and
  exit 1; old sessions refused once, old password hashes verify, login, logout, CSRF and rate
  limits; CLI usage identical to 0.21.0; TUI in a pty at several sizes; sandbox with every
  feature configured; kula-scan default runs clean.
- **Release artifacts.** `release.sh` dry run (12 artifacts, checksums verify, reproducible
  binaries), fresh installs and 0.21.0 → 0.22.0 upgrades of the .deb and .rpm on Debian 12/13,
  Ubuntu 24.04/26.04, AlmaLinux 9/10 and Fedora 43, `systemd-analyze verify` against systemd
  252, 257 and 261, OpenRC and runit, the container image, the pinned checksum of
  `install_v3.sh` in README, docs and landing, `install.sh` and `install_v2.sh` unchanged.
- **Dashboard in Chromium.** Every range at phone, laptop and 4K width, themes, layouts, Focus
  and TV mode, split cards, Graph Bounds, Rearrange, all 26 locales, the same 49 cards and 6
  gauges as 0.21.0, and every 0.21.0 browser preference honoured by 0.22.0 on the same origin.
  No exception, CSP violation or off-host request apart from §3.4.

## 5. Not covered

- The arm64 and riscv64 binaries were built and vetted, not executed (no user-mode emulator).
- The snap was not built; the packages were not run under a real systemd; `push.sh`,
  `install_v3.sh` and the AUR build were read, not run.
- Clock steps, an NVIDIA GPU, a host without utmp, the 1,024-container limit, hours-long soaks.
- Benchmarks against 0.21.0 were not run: the machine was loaded by the parallel tests.
- The fixes listed above were made after this test run and were not re-tested as part of it.
