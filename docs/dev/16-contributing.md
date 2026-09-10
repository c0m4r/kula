# Contributing

Thanks for your interest in Kula! Contributions are welcome.

## Getting started

Per [`.github/CONTRIBUTING.md`](../../.github/CONTRIBUTING.md):

> To contribute just open an issue or discussion on github and let me know about your ideas.

Starting with an issue or discussion lets the maintainer give direction before you invest time —
especially for anything touching the storage codec, security, or the public API.

## The golden rules

From [`AGENTS.md`](../../AGENTS.md):

1. **Build:** `./addons/build.sh`
2. **Test:** `./addons/check.sh` — runs `govulncheck`, `gofmt -l .`, `go vet`, `go test -v -race`,
   and `golangci-lint`, in that order. **All must pass** before a change is mergeable.

## Workflow

1. Fork and branch off `main`.
2. Make your change. Match the surrounding code's style, naming, and comment density.
3. Add/adjust tests (see [Testing & QA](12-testing.md)). New parsers should get a fuzz target;
   security-relevant changes should be covered by both unit tests and, where applicable,
   [kula-scan](13-kula-scan.md).
4. Run `./addons/check.sh` until green. Frontend/dashboard changes also need
   `./addons/test-frontend-regressions.sh` (Node.js 22+ and Chromium/Chrome).
5. If you touched Python helpers, run `black`, `pylint`, and `mypy --strict`.
6. Open a PR using the [template](../../.github/pull_request_template.md).

## Coding conventions

- **CGO-free.** The binary builds with `CGO_ENABLED=0`; don't introduce cgo dependencies.
- **Collectors return `nil` on failure** — never break the whole sample for one missing
  subsystem.
- **Storage codec is append-only and flag-gated.** Never insert a section between existing ones,
  never reuse a preamble flag bit. Follow [Adding a Metric Type](14-adding-metrics.md) exactly,
  and mirror any codec change in the Python decoder ([`inspect_tier.py`](../../addons/inspect_tier.py)).
- **Security defaults stay strict.** Don't loosen headers, CSRF, or sandbox rules by default;
  make relaxations opt-in via config.
- **Keep dependencies minimal.** Kula's value is being self-contained; weigh every new module.

## Where things live

See [Project Layout](02-project-layout.md). Common targets:

| You want to… | Edit |
|--------------|------|
| Add/adjust a metric | `internal/collector/` (+ codec, per [the guide](14-adding-metrics.md)) |
| Change the API or server | `internal/web/` |
| Change storage behavior | `internal/storage/` |
| Add a config option | `internal/config/config.go` + `config.example.yaml` |
| Change the dashboard | `internal/web/static/js/app/` |
| Add a translation | `internal/i18n/locales/` (see [i18n](11-i18n.md)) |
| Change the sandbox | `internal/sandbox/sandbox.go` |

## Documentation

When you change behavior, update the relevant page in this `docs/` tree and, if user-facing,
`config.example.yaml` and `README.md`. Operational deep-dives also live on the
[project wiki](https://github.com/c0m4r/kula/wiki).

## Governance & security

- **Code of Conduct:** [`.github/CODE_OF_CONDUCT.md`](../../.github/CODE_OF_CONDUCT.md).
- **Security policy / vulnerability reporting:** [`SECURITY.md`](../../SECURITY.md) — report
  privately rather than opening a public issue.
- **License:** contributions are under the [GNU AGPLv3](../../LICENSE).

## Releasing

Maintainer flow is documented in [Packaging & Release](15-packaging.md).
