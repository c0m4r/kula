# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Types of changes: Added, Changed, Deprecated, Removed, Fixed, Security

## [0.11.0] - TBA

### Added

- Storage v2: binary format replacing JSON in tier files, resulting in significant performance improvements
- Storage v1 to v2 migration feature
- Prometheus metrics endpoint: `/metrics` with optional token-based auth
- In-process query cache
- Health check endpoints (/status & /health)

### Deprecated

- Storage v1 (legacy JSON)

### Security

- Landlock ABI version check at startup

## [0.10.0] - 2026-03-17

### Added

- GPU monitoring
- New env var: KULA_LOGLEVEL for log level override
- Configurable version display

### Changed

- Tune and optimize argon2: 64 => 32MB, time 1 => 3
- Main app js split into 14 focused files

## [0.9.2] - 2026-03-15

### Fixed

- Handle virtual/unavailable disk temperature sensors reporting absolute zero (#9)
- Fix debug parse error messages being logged unconditionally regardless of log level (#9)

## [0.9.1] - 2026-03-15

### Security

- Add Origin/Referer validation for state-changing HTTP requests to provide additional CSRF protection
- SRI hashes and CSP nonce injection for javascript files
- Security headers hardening
- WebSocket message size guard in the frontend
- Fix WebSocket double-unregistration bug using sync.Once
- Implement WebSocket connection limits (global and per-IP) to prevent resource exhaustion
- Harden WebSocket connection counting logic with defensive decrement patterns
- Harden `X-Forwarded-For` trust by using the rightmost IP in the chain
- Add upper bound to history API `points` parameter to prevent resource exhaustion

## [0.9.0] - 2026-03-14

### Added

- Add missing HTTP Server Timeouts
- Support for env vars KULA_LISTEN and KULA_PORT

### Changed

- Substantial TUI rework
- New default port: 27960
- New default listen value: "" - listen on all v4/v6 addresses
- Make the program naming consistent

### Deprecated

- No longer listening on port 8080

### Fixed

- Prevent fmt.Sprintf with %s from producing invalid JSON
- Add missing request body size limit on login

## [0.8.3] - 2026-03-12

### Added

- Static Inter and Press Start 2P fonts

### Changed

- Change data dir default chmod to 750

### Fixed

- Fix filesystem mount detection in containers by reading both /proc/{1,self}/mounts (#5)

### Removed

- External fonts dependency removed

## [0.8.2] - 2026-03-12

### Added

- Debug logging for disk and network collector auto-discovery
- New logging level: `debug`

### Changed

- go: upgraded golang.org/x/crypto v0.48.0 => v0.49.0
- go: upgraded golang.org/x/text v0.34.0 => v0.35.0

### Fixed

- Disk and network collector auto-discovery fixes

## [0.8.1] - 2026-03-11

### Added

- Data compression for http and ws traffic (configurable, enabled by default)

## [0.8.0] - 2026-03-11

### Added

- Graph Y-axis max bound settings (#2)
- Configurable default aggregation (avg/min/max) in `config.yaml`
- Initial multi disk and network interfaces support (#5)
- Disk drives temperature monitoring (#2)
- Dashboard sections: "", "Thermals" (#2)

### Changed

- Format raw bytes to human readable format inside tooltips
- Update charmbracelet/colorprofile
- CPU temperature graph moved to the new "Thermals" section (#2)
- Multiple minor fixes

### Fixed

- Fix double-counting of Disk I/O from logical volumes (LVM, dm-crypt, mdraid)
- Fix non-monotonic timestamps causing spikes in dashboard after history load
- Fix inaccurate aggregation durations by using actual sample collection jitter instead of hardcoded 1s

## [0.7.5] - 2026-03-10

### Added

- Metrics aggregation (Avg/Min/Max)

### Changed

- Storage optimization: Round values to 2 decimal points
- Expanded graph is lava (move other graphs out of the way)
- Minor UX enhancements

## [0.7.4] - 2026-03-09

### Added

- New config option to set the default dashboard theme

### Fixed

- Disk monitoring inside docker containers (#5)

## [0.7.3] - 2026-03-08

### Added

- More installation methods
- Initial implementation of CPU thermal monitoring
- New config options to override hostname and enable/disable system info

### Changed

- Helper scripts enhancements
- Updated golang.org/x/sys module

### Fixed

- Clock sync check inside containers

## [0.7.2] - 2026-03-07

### Added

- First docker hub image release
- Space Invaders: new high score message and sound effect
- collector unit tests
- Light theme & ability to toggle light/dark mode (#1)

### Fixed

- tier aggregation gap bug at startup fix

### Security

- fixed web socket origin validation
- fixed secure cookie flag trust proxy validation
- fixed session token generation, improved session security
- Password hashing: Argon2 parameters and secrets in config
- go update to 1.26.1 to address GO-2026-4602, GO-2026-4601, GO-2026-4600, GO-2026-4599

## [0.7.1] - 2026-03-05

### Added

- dev console welcome message
- script checking available kula updates
- script checking available go module updates

### Changed

- inspect_tier.py: lint refactor
- startup time optimizations
- updated go module: clipperhouse/displaywidt
- updated go module: clipperhouse/uax29/v2

### Removed

- removed go module: clipperhouse/stringish

## [0.7.0] - 2026-03-04

### Added

- Built-in `kula inspect` CLI command to read and display tier files metadata
- Standalone python script `addons/inspect_tier.py` for parsing binary storage and extracting latest JSON samples
- Storage benchmark suite
- New unit tests
- New metrics displayed — Shmem, IOPS, TCP error rates, File Descriptors
- Expandable graphs

### Changed

- Storage size optimization: Round CPU and network values to 2 decimal places before saving
- Storage size optimization: Ignore `loop` and `tmpfs` devices
- Updated mock data generator
- Replace static disk usage chart with over time data
- Round `_pct` (percentage) values
- Improved performance
- Metrics slimdown — removed unused/negligible fields to reduce JSON payload and storage footprint

### Fixed

- Fixed hardcoded tier resolutions in web UI
- Less anoying tooltips

### Removed

- Non-linux filesystems
- Per-core CPU monitoring (for now)

## [0.6.0] - 2026-03-03

### Added

- Dynamic backend downsampling for long time windows (e.g. 1h) to improve UI performance and lower bandwidth
- Graph peak-bound rendering to ensure usage spikes are visible on all historical resolutions

### Changed

- dynamic gap detection algorithm handles variable chunk sizes natively

## [0.5.1] - 2026-03-03

### Changed

- move system info to a dropdown list
- display hostname in the title

### Fixed

- fix system info and alerts dropdown responsiveness

## [0.5.0] - 2026-03-02

### Added

- Dual-stack IPv4 and IPv6 support
- Storage directory fallback in case of insufficient permissions
- Chart.js static library

### Changed

- Logo / typography
- Chart.js updated to 4.5.1
- Updated helper scripts

## [0.4.2] - 2026-03-02

### Added

- OS, Kernel, Arch info

### Changed

- various graph and storage enhancements

### Fixed

- Version variable not being self-contained
- Responsive design improvements

## [0.4.1] - 2026-03-02

### Fixed

- Fixed 100% CPU exhaustion in browser when switching to 1h time window
- Fix zoom resolution on coarse-resolution time ranges

### Security

- Added rate limiting (max 5 attempts per 5 mins) to `/api/login` endpoint
- Added strict absolute path validation to prevent directory traversal in storage config
- Replaced silent parsing in `/proc` collectors with safe wrappers that explicitly log malformed data
- Updated daemon to gracefully shut down open network listeners using `context.Context` signal catching
- Migrated WebSocket handler from deprecated `golang.org/x/net/websocket` to `github.com/gorilla/websocket`
- Added strict `Origin` validation to prevent Cross-Site WebSocket Hijacking (CSWSH)

## [0.4.0] - 2026-03-01

### Added

- Landlock sandboxing implementation
- Logging of API requests
- Time range info when zooming in
- Mock data generator

### Changed

- Buffered I/O Streams (time windows switching optimization)

### Security

- Fixed XSS vulnerability in web UI system info display
- Fixed insecure Auth Session cookie by setting Secure attribute dynamically
- Replaced Whirlpool with argon2
- Fixed critical RLock and Delete map panic in ValidateSession
- Fixed weak session token generation crash
- Fixed storage tier permissions
- Added security headers (CSP, Frame-Options, Content-Type)
- Restrained WebSocket connections mapping MaxPayloadBytes limits
- Added missing upper bounds checks against abusive 31+ day historical queries
- Masked password with asterisks in the hash-password mode

### Removed

- Whirlpool password hashing algorithm

## [0.3.1] - 2026-02-28

### Changed

- unified the gauges-row of grid and list layout

### Fixed

- golangci-lint suggested fixes

## [0.3.0] - 2026-02-28

### Added

- Additional time presets
- Space Invaders

### Changed

- Default storage tiers sizes (old: 100/200/200 new: 250/150/50 MB)
- build.sh cross-compile enhancements
- RAM usage optimizations: max sample count cap QueryRangeWithMeta

## [0.2.1] - 2026-02-28

### Fixed

- Fixed: Archive tier 1 wrap bug

## [0.2.0] - 2026-02-28

### Added

- Cross-compile for amd64, arm64, risc-v
- Packaging helper scripts for Debian and Arch
- Man page
- Bash completion
- Docker integration
- Unit tests
- Focus mode
- New config option: web.join_metrics (default false)
- VERSION file

### Changed

- Pause stream on graph hover/zoom
- Do not display decimal point values for PPS and Disk I/O
- Do not join metrics (leave gaps for non-existing metrics)
- Clickable header
- Hardcoded version replaced with reading VERSION file
- Alert icon greyed out while no alerts raised

### Fixed

- Fixed: missing usage info in --help

## [0.1.0] - 2026-02-27

### Added

- First working version of kula-szpiegula
- Lightweight, real-time data collection
- Historical data tiers
- Live monitoring
- Web UI
- TUI
