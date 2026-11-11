# Building & Toolchain

## Prerequisites

- **Go** — the version pinned in [go.mod](../../go.mod).
- Optional dev tools used by `check.sh`:
  - [`govulncheck`](https://golang.org/x/vuln/cmd/govulncheck)
  - [`golangci-lint`](https://golangci-lint.run/)

The binary is **CGO-free** (`CGO_ENABLED=0`) and fully static.

## Quick builds

```bash
# Dev build (~21 MB, with symbols)
CGO_ENABLED=0 go build -o kula ./cmd/kula/

# Production build (~15 MB, ~5 MB xz-compressed)
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -buildvcs=false -o kula ./cmd/kula/
```

## The build script

[`addons/build.sh`](../../addons/build.sh) wraps the production build and reads the version
from the [`VERSION`](../../VERSION) file:

```bash
./addons/build.sh          # current architecture: kula, kula-scan and gen-mock-data
./addons/build.sh cross    # cross-compile amd64, arm64, riscv64 → dist/ (alias: all)
./addons/build.sh --help
```

Cross builds use:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=<arch> go build \
  -trimpath -ldflags="-s -w" -buildvcs=false \
  -o dist/kula-linux-<version>-<arch> ./cmd/kula/
```

Supported targets: `linux/amd64`, `linux/arm64`, `linux/riscv64`.

## The check script

[`addons/check.sh`](../../addons/check.sh) is the canonical pre-commit gate. It runs, in order:

1. **`govulncheck ./...`** — known-vulnerability scan (prefers `~/go/bin`, then system; skips
   with a hint if absent).
2. **`gofmt -l .`** — fails on any unformatted file (run `gofmt -w .` to fix).
3. **`go vet ./...`**.
4. **`go test -v -race ./...`** — full test suite with the race detector.
5. **`golangci-lint run ./...`** — linting (skips with a hint if absent).

```bash
./addons/check.sh
```

> All five checks must pass before merging. The
> [AGENTS.md](../../AGENTS.md) rules name this script as the test suite.

## Tests, fuzzing & benchmarks

```bash
# Unit tests with the race detector
go test -race ./...

# Storage benchmark suite (default 3s per bench, pretty output)
./addons/benchmark.sh

# Fuzz targets
./addons/fuzz.sh

# Browser frontend regressions (needs Node.js 22+ and Chromium/Chrome)
./addons/test-frontend-regressions.sh
```

See [Testing & QA](12-testing.md) for what's covered.

## Updating dependencies

```bash
./addons/go_modules_updates.py           # report outdated used modules and their release dates (check-only)
go get example.com/module@v1.2.3         # the versions it lists as at least 14 days old
go mod tidy
```

Go has no release-age window like npm's `min-release-age` (below): `go get -u` takes the newest
release of every dependency, however recent, and a hijacked release is usually caught within
days. Update to named versions at least 14 days old, the window the npm side enforces;
`go_modules_updates.py` prints each update's release date, marks the younger ones to wait, and
prints the `go get` line for the rest. Check what they pull in with `git diff go.mod`. A fix
`govulncheck` reports for Kula is the exception: take it when it is out. `go.sum` and the
checksum database keep a version from changing once fetched; they cannot tell a malicious
release from a good one.

There are companion scripts: [`addons/chartjs-updates.py`](../../addons/chartjs-updates.py)
(report newer Chart.js/plugin releases than the bundled ones), and
[`addons/update.py`](../../addons/update.py) (compare the local `VERSION` with the latest GitHub
release).

The vendored Chart.js bundle is rebuilt with Node.js and npm, which building Kula does not need:

```bash
./addons/build-chartjs.sh           # rebuild from addons/chartjs/package-lock.json
./addons/build-chartjs.sh --update  # bump chart.js, chartjs-plugin-zoom and esbuild first
```

npm runs with supply-chain guards, set in `addons/chartjs/.npmrc` and passed again on the command
line so `npm_config_*` environment variables cannot weaken them:

- `ignore-scripts=true`: no package install scripts run.
- `min-release-age=14`: `--update` resolves only releases at least 14 days old.
- Exact pins: `package.json` pins each direct dependency (`save-exact=true`), and the lockfile
  pins every transitive one with a sha512 integrity hash.

`npm ci` installs locked versions without applying `min-release-age`, and it fetches whatever
tarball an entry's `resolved` URL names, accepting it when it matches the entry's `integrity`.
Before anything is installed, `addons/chartjs/verify-lock.js` therefore checks every locked
package against `registry.npmjs.org`, including the optional esbuild binaries for other
platforms:

- Its `resolved` URL and sha512 `integrity` must be exactly the `dist.tarball` and
  `dist.integrity` the registry lists for the entry's name and version, so npm can fetch and
  accept that release and no other. A `resolved` URL that `new URL()` would rewrite (dot
  segments, `%2e`, backslashes) or that carries a query or a fragment is refused, as are npm
  aliases, links and lockfile paths that are not a `node_modules/` chain of package names.
- That release must have been published at least 14 days ago.
- Every direct dependency, in any of `dependencies`, `devDependencies`,
  `optionalDependencies` and `peerDependencies`, must be an exact pin with a locked entry.
- A lockfile older than v2 has no `packages` map to check and is rejected outright rather than
  passing with nothing checked.

`node addons/chartjs/verify-lock.test.js` runs these checks offline, against a stand-in
registry and crafted lockfiles. Run npm by hand in `addons/chartjs/` only; its `.npmrc` applies
the same guards.

Rebuilding from the lockfile reproduces `chartjs-bundle.min.js` byte for byte, and CI holds it
to that: the `chartjs-bundle` job of `ci.yml` runs the `verify-lock.js` tests, rebuilds the
bundle with `./addons/build-chartjs.sh` and fails when the committed file differs, so a change
to its minified bytes cannot pass unseen. `./addons/ci-local.sh run ci chartjs-bundle` runs the
same job locally. After an update, run the browser regressions: the dashboard relies on a
private Chart.js method (see [Frontend](10-frontend.md)).

## Go formatting & lint

[`.golangci.yml`](../../.golangci.yml) enables the `gofmt` and `goimports` formatters, with
`local-prefixes: kula` so project imports form their own group. `gofumpt` is deliberately disabled
(the module path `kula` has no dot, so gofumpt classifies project imports as stdlib and fights
goimports).

## Python helper linting

The Python operator scripts are formatted/linted strictly:

```bash
black addons/*.py
pylint addons/*.py
mypy --strict addons/*.py
```

## Packaging

Distro and container package builders live in `addons/` — see [Packaging & Release](15-packaging.md):

```bash
./addons/build_deb.sh      # → dist/kula-*.deb
./addons/build_rpm.sh      # → dist/kula-*.rpm
./addons/build_aur.sh      # → dist/kula-<version>-aur/ (then makepkg -si)
./addons/build_snap.sh     # → dist/kula-*.snap (needs snapcraft + LXD)
./addons/build_appimage.sh # → dist/kula-<version>-<arch>.AppImage (needs appimagetool)
./addons/docker/build.sh   # Docker image
```

## CI

GitHub Actions workflows live in [`.github/workflows/`](../../.github/workflows/):

- `ci.yml` — build + the `check.sh`-style verification (govulncheck, `go vet`, race tests,
  golangci-lint; no standalone `gofmt` step), then a `./kula --version` smoke test.
- `frontend.yml` — the three Chromium fixtures, each in its own timed step.
- `semgrep.yml` — static analysis security scan.
- `zizmor.yml` — static analysis of the workflows themselves, run when anything under `.github/`
  changes. [`.github/zizmor.yml`](../../.github/zizmor.yml) limits `uses:` to `actions/*`.

[`addons/ci-local.sh`](../../addons/ci-local.sh) runs the same workflows locally, inside a Docker
image that stands in for the `ubuntu-latest` hosted runner: the same Ubuntu release, a non-root
`runner` user with passwordless sudo, Google Chrome, and a toolcache holding the Go, Node.js and
Python versions the workflows ask for (read from `go.mod` and the workflow files, so bumping a
version there is enough; anything but a plain version number such as `1.26.8`, `22` or `3.x` is
rejected):

```bash
./addons/ci-local.sh                  # list workflows and their jobs
./addons/ci-local.sh run ci           # every job of ci.yml
./addons/ci-local.sh run frontend     # the browser job, Chrome included
./addons/ci-local.sh run-all          # every job of every workflow
./addons/ci-local.sh shell            # poke around inside the runner image
./addons/ci-local.sh --rebuild run ci # re-resolve the toolcache versions first
```

The working tree (tracked plus untracked, minus ignored files) is snapshotted into the container
and committed there, so a job sees uncommitted changes; Go module, build, npm and pip caches
live in named volumes owned by the container's `runner` user, so only the first run pays for them
(`--clean-cache`, or `clean`, starts over). [`addons/ci-local/run-workflow.py`](../../addons/ci-local/run-workflow.py) executes the
steps: it implements the first-party actions the workflows use (`actions/checkout`,
`actions/setup-go`, `actions/setup-node`, `actions/setup-python`), honours `if`, `env`,
`working-directory`, `shell`, timeouts, `continue-on-error` and the
`GITHUB_PATH`/`GITHUB_ENV`/`GITHUB_OUTPUT` command files, and fails loudly on a construct it does
not emulate rather than skipping the step. Conditions follow GitHub's rules: `true`/`false`/`null`
literals, loose equality (mismatched types compare as numbers), an implicit `success() && …`
unless the condition calls a status function, and a failed step skips the rest of the job except
`always()`/`failure()` steps; a job-level `if` is evaluated too. `steps.<id>.outcome` is a step's
own result and `.conclusion` that result after `continue-on-error`, and `run` scripts see the
same contexts as `if`. `run-all` runs a workflow's jobs in `needs:` order and skips a job whose
needed job failed. A `strategy.matrix` with more than one combination, `include` or `exclude` is
not emulated. An expression that reads a context ci-local does not provide (`needs`, for
instance) fails the job instead of evaluating to empty. The container holds no token, so the
workflows' read-only security model holds trivially; steps that genuinely need GitHub (a push,
a release upload) cannot work here by design.
The container is capped at 8 GiB of memory and 4096 processes so a runaway step cannot take the
workstation down; `CI_LOCAL_MEMORY` (e.g. `16g`) and `CI_LOCAL_PIDS` change the ceilings, and
`none` lifts either. `--step PATTERN` runs only the `run` steps whose name matches, which is the fast way to iterate
on one failure; the `uses:` setup steps always run, since they put the toolchains on `PATH`.

What the container can reach: a read-only snapshot of the tree (tracked and untracked files,
minus ignored ones — mind a stray `.env`), the three cache volumes, and the network. It gets no
Docker socket, no host mounts and no host environment beyond the `-e` values you pass. The
`runner` user has passwordless sudo for fidelity with the hosted runner, so container root is one
step away, and without user-namespace remapping container root is host root to the kernel; to run
a branch you do not trust, use rootless Docker or `userns-remap`, and `--clean-cache` afterwards
so the shared Go and npm caches it could have written are not reused. `addons/ci-local/` itself
belongs to the checkout, so an untrusted branch also brings its own image definition. The image
build verifies Go and Node.js archives against the checksums their publishers list, and pins
Google's package signing key by fingerprint (`GOOGLE_LINUX_SIGNER_FPR`).

Next: [Collector Subsystem](04-collector.md).
