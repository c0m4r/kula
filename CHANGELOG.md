# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Types of changes: Added, Changed, Deprecated, Removed, Fixed, Security
(a release that needs action when upgrading opens with Upgrade notes)

## [0.22.0] - TBA

### Upgrade notes

- Everyone signs in again once
- Take a backup if you may downgrade: older versions can't read the new tier records
- Without auth, and for `/metrics` scraped without a bearer token, list the name you reach Kula by in `web.security.allowed_hosts` unless it is local (reverse proxies that forward `Host`, Tailscale, internal DNS)
- Startup now stops on an unknown config setting, a `-config` path that cannot be read or created, an `allowed_origins` entry other than a plain `scheme://host[:port]`, or a zero `session_timeout`, WebSocket limit or `argon2` time or threads
- PgBouncer in front of PostgreSQL needs `ignore_startup_parameters`
- Docker: mount `/run/utmp` or `/run/systemd/sessions` to keep the logged-in user count
- Hosts with dozens of containers, disks or interfaces may need a higher `MemoryMax` in a systemd drop-in

### Added

- Rearrange gauges and chart cards in the Customization menu (#51)
- Prometheus: current Apache2 rates `kula_apache2_accesses_per_second` and `kula_apache2_served_bytes_per_second`
- MySQL over TLS with `sslmode` and `sslrootcert`
- `install_v3.sh` installer
- kula-scan reads the password from `KULA_SCAN_PASSWORD`

### Changed

- History queries read only their time range (TUI too), and restarts are faster: each tier keeps a small `tier_N.idx` seek index
- Rolling views refresh only their newest buckets instead of the whole range
- History queries, compressed responses and container lists use far less memory
- Stored records carry a checksum, and backups no longer pause collection or the dashboard
- Tier 1 and 2 rollups align to wall-clock minutes, so rolling and past views agree
- `/api/history` accepts an optional `tier` hint, and API and WebSocket errors are always JSON
- History requests take turns; dashboards retry one the server was too busy for and reconnect at staggered times after a restart
- Disk temperatures and power supplies are read every 10 s, set with `collection.disk_temp_interval` and `collection.power_supply_interval`
- PostgreSQL table health, container details, GPU detection and System Info sensors are read far less often
- Only containers with a full runtime ID are monitored, up to a limit per sample
- Charts are built once on page load, and Graph Bounds apply without reloading them
- Cards keep one consistent order everywhere, and split charts expand and collapse like every other chart
- Apache2 charts and the AI assistant show current request and byte rates
- The login form tells a wrong password apart from a rate limit or a busy server
- Unknown settings and ones that would break login or WebSockets stop startup, and `allowed_origins` is normalized
- `kula_system_logged_in_users` is omitted when the host has neither utmp nor logind sessions
- systemd: the unit caps Kula's memory and no longer sets `ProtectClock`, which kept it from reading the clock
- A setting the sandbox cannot grant is left out with a warning instead of disabling the whole sandbox
- Failed WebSocket writes are logged only at debug level, and the TUI prints its log lines when it exits
- kula-scan: a default run signs in once and logs out; account enumeration needs `-aggressive`

### Deprecated

- Prometheus `kula_apache2_requests_per_second` and `kula_apache2_bytes_per_second` (use `kula_apache2_accesses_per_second` and `kula_apache2_served_bytes_per_second`)

### Fixed

- Disk utilization is measured again (read 0% since 0.7.0)
- TCP retransmissions are measured and stored in history, and the first errors and resets after a quiet boot are counted
- Clock sync with a local-time RTC, mounts with a space in their path, and used memory and swap are measured correctly
- `web.graphs.gpu_temp` takes effect
- Logged-in user count works under the sandbox and on arm64, with a systemd-logind fallback
- Containers: rootless Podman is found for the user Kula runs as, hosts with hundreds of containers keep their metrics, and a socket that refuses connections is no longer reported as missing
- PostgreSQL and MySQL monitoring no longer piles up server connections; MySQL passwords with `@`, `/` or `:` and IPv6 hosts work
- PostgreSQL: polls on a frozen server are cut off, settings are quoted, and a standby of a quiet primary no longer shows a growing replication lag
- nginx, Apache2, PostgreSQL, MySQL and container rates no longer spike after a restart or failed poll
- A hung network mount or a slow nginx or Apache2 status page no longer stalls collection
- nginx and Apache2 failures are logged when they change, and redirects are not followed
- `https://` targets, an Ollama URL without a port and PostgreSQL `verify-ca`/`verify-full` work under the sandbox
- History no longer goes missing after a tier wraps, a crash, a restart or a clock step back, and long views keep 1-minute detail right after a restart
- Damaged tier files, records from a newer version, infinite, NaN or out-of-range values and over-long names or lists no longer hang or break history
- Custom metric values beyond the 32-bit float range are refused and logged once
- Shutdown waits for the sample being collected before closing storage
- Tier 1 and 2 rollups no longer carry needless statistics, so they retain more history
- `inspect_tier.py` walks damaged tiers the way Kula does and answers `--help`
- Battery/UPS, PostgreSQL replication and application charts no longer end up in the wrong place
- Focus mode no longer hides split temperature charts or an application-only selection, or pins the crosshair of charts being selected
- Split charts no longer merge devices whose names differ only in punctuation
- A malformed aggregation or Focus mode preference no longer blanks the dashboard
- An expired session returns to the login screen instead of reconnecting forever, and AI panel actions no longer run twice after signing in again
- Alerts, application and battery charts, card subtitles, device names and login, AI panel and disk messages are translated; split charts and the sampling tooltip follow a language change
- Rolling views no longer stay marked "partial" after a late refresh or announce every refresh to screen readers
- Live views follow the server's clock when the browser's is off, and a background tab no longer piles up chart memory
- AI assistant: long answers no longer freeze the tab, long conversations are no longer refused, and the metrics tool keeps to the history range limits
- Bearer tokens work with auth status and logout
- `/metrics` writes each metric family once, with valid UTF-8 label values, so `promtool` accepts it
- `kula hash-password` refuses an empty password, answers `--help` and keeps spaces in a piped password
- runit: logging no longer blocks Kula

### Security

- DNS rebinding protection: with auth disabled, and on `/metrics` without a token, only local host names and `web.security.allowed_hosts` are answered
- Sessions are tied to the account password, and login is refused while auth is disabled
- Concurrent logins and history requests can no longer exhaust CPU or memory, and a history request can no longer read the whole raw tier
- Login rate limits count an IPv6 client by its /64 and a NAT64 one by its IPv4 address; behind `trust_proxy` they use the proxy's own `X-Forwarded-For` entry, and only a valid address
- Startup warnings when `allowed_origins` without `origin_validation` exposes the live stream, or a `unix_socket` without `trust_proxy` gives every client one login limit
- A `unix_socket` whose mode cannot be set is not served, and the custom metrics socket limits its connections
- Content-Security-Policy no longer allows inline styles, pins `base-uri` and `form-action`, and lets only the game page reach the score URL
- Every dashboard module loads with an integrity check
- Dashboard pages and API responses are never cached, and signing in clears the password field
- AI proxy: connects only to loopback, follows no redirects, forwards only user and assistant turns on a listed model, limits tool calls and concurrent chats, and keeps backend errors from the client
- Logs, the TUI and kula-scan escape text from requests, monitored servers and scan targets; logs hide credentials in status URLs, and kula-scan bounds what a target streams
- The sandbox confines Unix sockets, UDP and signals on newer kernels, opens storage under it, and covers `kula tui`
- Local users can no longer plant fake containers, and a root Kula no longer uses another user's Podman socket
- A fake database server can no longer crash Kula, and crafted tier files can no longer hang or exhaust it, the TUI or `inspect_tier.py`
- Storage, backup, session and GPU log files are never opened through a link or FIFO
- `.deb` and snap configs are no longer world-readable or writable by the service, upgrades leave a linked config alone, and release tarballs no longer hand `/opt/kula` to the builder's uid
- Docker: the container drops all capabilities and runs read-only, and root owns the binary and config
- The documented installer check runs only an installer that passed it
- The Chart.js lockfile must match the registry, CI rebuilds the bundle from it, and `go_modules_updates.py` holds back freshly released updates
- Vulnerabilities are reported privately

## [0.21.0] - 2026-10-01

### Added

- Clock picker for custom range times
- Download the chart as PNG image
- CSS and JS assets auto-minification at startup with `web.minify_assets: true`
- TV mode under Focus Mode
- Space Invaders: graphics settings and restart option
- tui: historical data charts

### Changed

- The custom range picker uses one month calendar instead of two side-by-side
- Retained history now limits only which days can be picked
- Unused CSS/JS cleanup
- The dashboard now loads one trimmed Chart.js bundle instead of three scripts
- The dashboard paints sooner: Chart.js no longer blocks the first paint, and charts are
  built once the page is on screen
- The Inter font ships as WOFF2 (349 KB instead of 875 KB, same glyphs)
- Static assets are revalidated with an ETag instead of downloaded again on every visit;
  an unchanged server answers 304 Not Modified
- tui: UI/UX improvements

### Fixed

- The Focus Mode bar ("Select graphs…", Hide gauges, Done, Cancel) is now translated
- Space Invaders: fixed game stability on firefox
- Layout shifts while the dashboard loads: the gauges, chart headers and sampling info no
  longer push the page down when they fill in
- A light-theme dashboard no longer shows a dark first frame; `global.default_theme` also
  applies before the config request finishes
- System Info no longer grades fans and voltages with temperature thresholds, and uptime refreshes
- System Info no longer stretches a lone section tab
- The remaining hard-coded dashboard strings are translated
- A custom range time inside a DST gap now shows the time that actually loads
- A corrupt record length in a tier file no longer allocates up to the whole ring before the read fails

## [0.20.3] - 2026-09-29

### Fixed

- Min/Max charts stopped drawing new data on short live windows (for example 5 minutes on a
  narrow screen or a list-layout card) whose history the server returned in bucketed form:
  live samples without extrema were appended as blank points and flagged "Min/Max unavailable".
  The dashboard now streams only onto native raw views and refreshes bucketed views instead
- Selecting a window without Min/Max (such as raw history or an empty refresh) no longer
  silently switches the rest of the session to Avg; the Min/Max choice returns as soon as a
  view supports it again

## [0.20.2] - 2026-09-20

### Fixed

- Avg/Min/Max availability flickering on raw history windows: clipping the right edge
  no longer certifies synthesized extrema, so native-resolution responses consistently
  stay `data`-only instead of depending on whether the lookahead scan saw a newer record
- Raw history windows no longer report "Min/Max unavailable" in detailed tooltips when
  the response never offered Min/Max aggregation

## [0.20.1] - 2026-09-18

### Added

- `global.show_system_details` (default `false`) gates the storage, network,
  connected-device and sensor inventory on the System Info page

### Changed

- System info no longer collects or displays IP and MAC addresses

## [0.20.0] - 2026-09-18

### Added

- System info page with `/api/system-info` endpoint
- Persistent disk IDs from sysfs, stable disk selectors and `kula_disk_info` mappings (#46)
- `kula disks` helper to list available disks and partitions with their persistent IDs (#46)
- Live, Back, Forward and Zoom out navigation with shareable historical ranges
- Drag-selection, Ctrl+wheel and touch pan/pinch
- Keyboard navigation and a shared crosshair with timestamp pinning
- Min–Max bands and detailed history tooltips with exact timestamps and coverage
- Local/UTC display and optional Data/CSV controls in Customization

### Changed

- Disk Prometheus `device` labels now use persistent IDs (#46)
- Smoother chart rendering and lower-memory history queries
- Custom and zoomed ranges, including device selections, stay frozen while live status updates
- Long live windows retain the selected history during refreshes and failures
- Zoom-in stops at 12 data points, excluding gaps, with a sampling-aware minimum duration
- Fresh installs default to Avg with straight, unfilled lines and one principal Min–Max band
- Grid/list changes preserve chart data and legend selections
- Tier naming unification
- Inspect feature improvements
- Implemented a unified rounding campaign while preserving full precision in storage and APIs
- UI performance improvements
- Aggregation optimization

### Fixed

- Expired live sessions now return to login and cancel pending history loads
- Signing in restores the selected range without reloading the page
- Disk history and rates mixing physical drives when kernel device names change;
  old name-only history remains separate from new identified series
- History aggregation, bucket boundaries and coverage across storage tiers, collection gaps
  and missing metrics; older rollups remain approximate
- Avg/Min/Max controls hidden by legacy history: restore supported CPU, load, memory and
  swap extrema through read-time compatibility without rewriting tier files; unsupported
  extrema remain gaps, with availability notices and source details in tooltips and CSV exports
- History caching and point limits, including sub-second requests and concurrent updates
- Historical points, extrema and sensor identities lost during zoom, redraws or live updates;
  missing observations now retain line breaks and shaded gap indicators
- Slow history responses overwriting newer ranges or active zoom gestures
- Date-picker, Focus Mode, chart-label, localization and crosshair accessibility issues
- Crashes on WebSocket disconnects, short container IDs and malformed history, plus spurious
  WebSocket errors on logout
- Increased `storage.tiers[].max_size` now takes effect without discarding history;
  shrinking an existing tier still requires manual action
- fix: bound offscreen chart state and release split cards (#50)

### Security

- Fixed login rate limiting that allowed targeted account lockout and global login denial
- Added an absolute session lifetime to prevent indefinite renewal

## [0.19.0] - 2026-08-31

### Added

- customization menu for appearance and accessibility preferences

### Changed

- TUI redesign
- golang and deps updates
- container metrics: one multi-series chart per metric type with app filter
- dark theme readability (#45)

### Fixed

- battery graph out of order after exiting focus mode
- battery/PSU metrics not being written to storage
- missing `split_by_device` translation
- frosted top bar

## [0.18.8] - 2026-07-29

### Changed

- sticky time range controls (#41)
- update github.com/mattn/go-isatty v0.0.22 => v0.0.24
- update github.com/mattn/go-runewidth v0.0.24 => v0.0.27

### Fixed

- add missing expand button to dynamically created charts (#40)
- keep mobile time preset menus usable on short viewports

## [0.18.7] - 2026-07-26

### Added

- container monitoring: support Podman Quadlet cgroups (#38)

### Fixed

- keep container chart history across recreate (#39)

## [0.18.6] - 2026-07-11

### Added

- New config option: game_score_url

## [0.18.5] - 2026-07-10

### Changed

- ignore fs type constraint when mountpoints is specified (#35)

### Fixed

- remove systemd ProtectHome setting that prevents /home diskspace monitoring (#33)

## [0.18.4] - 2026-06-16

### Added

- custom metrics: `applications.custom_stale_after` to tune how long a group
  keeps reporting after its producer stops (default: a few collection intervals)

### Fixed

- storage: tolerate unclean-shutdown holes in tier reads
- custom metrics: stop displaying a group's last values once its producer stops
  pushing — stale feeds now leave a gap instead of a frozen line
- custom metrics: dedup repeated metric names per message (last value wins),
  preventing duplicate Prometheus series and rejected scrapes
- custom metrics: data race on the debug flag; surface oversized/failed socket
  reads instead of dropping them silently

### Changed

- custom metrics: reap idle socket connections after 5m; emit values in config order

## [0.18.3] - 2026-06-09

### Changed

- plot container memory usage only, move limit to subtitle (#31)

### Fixed

- container memory usage and limit now match docker stats (#31)

## [0.18.2] - 2026-06-06

### Added

- example config seeding

## [0.18.1] - 2026-06-06

### Added

- Time window/range/agg in URLs co-authored by @apre

### Fixed

- revert the monotonic-timestamp clamp
- refuse to start when an explicitly specified `-config` file is missing

## [0.18.0] - 2026-06-04

### Added

- Scheduled backups of the storage tier files
- Missing access logs for served web UI content

### Fixed

- tier wrap fixes (#24)

### Changed

- gen-mock-data update and refactor
- tier files intergrity improvements
- logging minor improvements

## [0.17.3] - 2026-06-01

### Added

- fuzz and runtime security tests
- kula-scan: active black-box safeguard scanner

### Changed

- Performance: query-cache entries expire by TTL instead of a full per-second rebuild

### Fixed

- Out-of-bounds panic decoding truncated or corrupt storage records

### Security

- Cap rate-limiter memory to prevent unbounded growth from distinct-IP floods
- Fix Login timing side-channel / username enumeration
- Fix out-of-bounds panic in the process collector

## [0.17.2] - 2026-05-30

### Changed

- Performance: ~60% fewer allocations and syscalls in the per-second process collector
- Performance: skip the WebSocket sample marshal when no clients are connected
- Performance: stop parsing /proc/stat once past the CPU block

## [0.17.1] - 2026-05-29

### Added

- Storage-independent collector micro-benchmarks
- MySQL/MariaDB replication monitoring enhancements

### Changed

- Performance: reduced CPU and allocation overhead of the per-second collection loop

## [0.17.0] - 2026-05-28

### Added

- Add missing PSU and Applications prometheus metrics
- Add postgres and mysql/mariadb replication monitoring

## [0.16.2] - 2026-05-27

### Security

- Fix: explicit second-character check (CWE-601)

## [0.16.1] - 2026-05-27

### Added

- Configurable base path support (#25) co-authored by @apre

### Changed

- Use shorter containers ID in cgroups mode (#28)

## [0.16.0] - 2026-05-20

### Added

- MySQL database monitoring
- Apache httpd server monitoring (#17)
- Unix socket support for the web server (#26)
- Config options to customize security features (#21)
- New env var: KULA_POSTGRES_PASSWORD
- Reverse proxy helper script (addons/reverse_proxy.py)

### Fixed

- PostgreSQL password is now properly single-quoted with libpq escaping
- Fix pg_stat_bgwriter query for PostgreSQL 17+ (split into pg_stat_checkpointer / pg_stat_io)
- Fix duplicate aggregation logic for minSample/maxSample
- Fix inspect reporting false information when tier is full (#24)
- Add missing permissions check for containers socket (#22)

## [0.15.0] - 2026-04-20

### Added

- AI assistant via Ollama API
- config option to disable space invaders [#14]
- auth: multiple users support [#15]

### Changed

- Add collection.interval allowed values list
- Checking collection.interval and storage.tiers consistency
- Tier resolutions ratio limit
- Check prefers-color-scheme to honor user's requested theme
- Default global.default_theme changed to auto

### Fixed

- fix storage directory handling being inconsistent
- fix login throttling being only IP-based
- fix argon2 mismatch with example config
- fix: don't leak internal error details from /api/history
- fix: sandbox log messages now match actual RWFiles/RWDirs permissions
- fix: data race on WebSocket client.paused field in broadcast
- fix: close WebSocket sendCh on client disconnect
- other minor bugfixes

### Security

- HSTS, CSP update, minor safety fixes

## [0.14.0] - 2026-03-29

### Added

- Applications monitoring: nginx, postgresql
- Containers monitoring: docker/podman with raw cgroups fallback
- Custom metrics monitoring via unix socket
- New build dependency: github.com/lib/pq
- TCP retrans metric added to network monitoring
- Initial PSU / Battery monitoring
- Dashboard search

### Changed

- Default value for default_aggregation to max
- Dashboard gauges modifications, sparkline bg charts for up/dl

### Fixed

- bugfix: custom collection.interval handling (#13)

## [0.13.0] - 2026-03-24

### Added

- Per-device/interface graph splitting
- New config option: mounts_detection - control how mount points are detected. (#5)
- New env var: KULA_MOUNTS_DETECTION for mounts_detection override (#5)
- New env var: KULA_DIRECTORY for directory override

### Changed

- Focus mode enhancements, option to hide gauges
- Refine gauages responsiveness
- ES6 Module Architecture refactor

### Security

- CSRF token protection

## [0.12.1] - 2026-03-22

### Changed

- Dashboard time axis improvements

### Fixed

- Fix zoom feature regression

## [0.12.0] - 2026-03-22

### Added

- Multilingual support for 26 languages

## [0.11.0] - 2026-03-20

### Added

- Storage v2: binary format replacing JSON in tier files, resulting in significant performance improvements
- Storage v1 to v2 migration feature
- Prometheus metrics endpoint: `/metrics` with optional token-based auth (disabled by default)
- In-process query cache
- Health check endpoints (/status & /health)
- New config option: web.prometheus_metrics
- New config option: web.ui - enable/disable web ui/dashboard

### Deprecated

- Storage v1 (legacy JSON)

### Security

- Landlock ABI version check at startup

### Fixed

- Config web.enable having no effect

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

- Fixed: Archive tier 0 wrap bug

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
