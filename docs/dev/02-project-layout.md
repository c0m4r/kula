# Project Layout

```
kula/
├── cmd/
│   ├── kula/               # Main binary entrypoint
│   │   ├── main.go         # Subcommand dispatch, serve/tui loops, password prompt
│   │   ├── disks.go        # `kula disks` listing of disks/partitions + persistent IDs
│   │   └── system_info.go  # OS name + kernel version readers
│   ├── kula-scan/          # Black-box security scanner (separate binary)
│   └── gen-mock-data/      # Mock timeseries generator for storage tests
│
├── internal/
│   ├── collector/          # Metrics collection engine
│   ├── config/             # YAML config parser + validator
│   ├── i18n/               # Embedded locale JSON + lookup
│   │   └── locales/        # 26 locale files (ar, de, en, es, fr, ...)
│   ├── sandbox/            # Landlock LSM enforcement
│   ├── storage/            # Tiered ring-buffer storage engine
│   │   └── testdata/       # Fuzz corpus
│   ├── sysinfo/            # Live hardware inventory for /api/system-info
│   ├── tui/                # Bubble Tea terminal UI
│   ├── backup/             # Cron-scheduled tier-file backups
│   └── web/                # HTTP server, API, WS, auth, Ollama, Prometheus
│       └── static/         # Embedded SPA (HTML/CSS/JS), fonts, icons
│
├── addons/                 # Build / test / packaging / ops scripts
│   ├── build.sh            # Build + cross-compile
│   ├── check.sh            # govulncheck + gofmt + vet + race tests + golangci-lint
│   ├── benchmark.sh        # Storage benchmark suite
│   ├── fuzz.sh             # Run the fuzz targets
│   ├── test-frontend-regressions.sh # Browser frontend regression tests
│   ├── install.sh          # Legacy guided installer
│   ├── install_v2.sh       # Current guided installer
│   ├── build_deb.sh        # Debian/Ubuntu package builder
│   ├── build_rpm.sh        # RHEL/Fedora package builder
│   ├── build_aur.sh        # Arch AUR package builder
│   ├── build_snap.sh       # Snap builder
│   ├── build_appimage.sh   # AppImage builder
│   ├── release.sh          # Release orchestration
│   ├── inspect_tier.py     # Standalone Python tier-file decoder
│   ├── reverse_proxy.py    # Test proxy for the Unix-socket listener
│   ├── go_modules_updates.py / update.py / chartjs-updates.py
│   ├── ansible/            # Ansible deployment role
│   ├── bash-completion/    # Bash completion script
│   ├── docker/             # Dockerfile + compose + push scripts
│   ├── init/               # systemd, OpenRC, runit service files
│   ├── man/                # man page (kula.1)
│   └── packaging/          # Packaging helpers (font/game removal)
│
├── scripts/                # Operator examples (nvidia-exporter.sh, custom_example.py)
├── landing/                # kula.ovh landing page
├── reviews/                # Historical review documents per version
├── dist/                   # Build output (packages)
│
├── config.example.yaml     # Fully-commented config template
├── config.yaml             # Local working config (gitignored content)
├── go.mod / go.sum         # Go module definition
├── version.go              # //go:embed VERSION → kula.Version
├── VERSION                 # Current version string
├── CHANGELOG.md            # Detailed changelog
├── README.md               # Project README
├── AGENTS.md               # Instructions for AI agents (commands + invariants)
├── SECURITY.md             # Security policy
├── LICENSE                 # GNU AGPLv3
└── .github/                # CI workflows, issue/PR templates, governance
```

## The `internal/` packages

Everything substantive lives under `internal/`, so it can't be imported by external modules.
Each package is documented in its own page:

| Package | Page |
|---------|------|
| `collector` | [Collector Subsystem](04-collector.md) |
| `config` | [Configuration reference](../user/04-configuration.md) (user) |
| `storage` | [Storage Engine](05-storage-engine.md) + [Codec](06-storage-codec.md) |
| `web` | [Web Server & API](07-web-server-api.md) + [Security](08-security.md) |
| `sandbox` | [Landlock Sandbox](09-sandbox.md) |
| `tui` | [Frontend](10-frontend.md) |
| `i18n` | [Internationalization](11-i18n.md) |
| `backup` | [Backups](../user/12-backups.md) (user) |
| `sysinfo` | [Web Dashboard](../user/05-web-dashboard.md) (user) |

## Version embedding

[version.go](../../version.go) uses `//go:embed VERSION` to bake the version string into
`kula.Version`, which `cmd/kula/main.go` reads as `var version = kula.Version`. Bump the
[`VERSION`](../../VERSION) file to release a new version; the build script reads it for package
names too.

## The binaries

The module produces three binaries but ships only one:

- **`./cmd/kula`** — the released binary (and Docker image).
- **`./cmd/kula-scan`** — a developer/operator security scanner that imports nothing from
  `internal/`; not in releases but covered by `go vet ./...` and `go test ./...`. See
  [kula-scan](13-kula-scan.md).
- **`./cmd/gen-mock-data`** — the mock-history fixture generator used by storage testing; not
  in releases, but covered by the same gate. See [Testing](12-testing.md#realistic-mock-history).

Next: [Building & Toolchain](03-building.md).
