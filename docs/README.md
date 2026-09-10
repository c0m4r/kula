# Kula Documentation

**Kula** is a lightweight, self-contained Linux® server monitoring tool. It collects system
metrics every second by reading directly from `/proc` and `/sys`, stores them in a built-in
tiered ring-buffer storage engine, and serves them through a real-time web dashboard, a
terminal UI, and a Prometheus exporter — all from a single dependency-free binary.

> Zero dependencies. No external databases. Single binary. Just deploy and go.

This documentation is split into two tracks. If you want to **run and operate** Kula, start
with the User Guide. If you want to **understand, build, or contribute to** Kula, head to the
Developer Guide.

---

## 📚 User Guide

For operators and administrators who deploy and use Kula.

| # | Document | Description |
|---|----------|-------------|
| 1 | [Introduction](user/01-introduction.md) | What Kula is, what it collects, and how it works |
| 2 | [Installation](user/02-installation.md) | Every install method: binary, Docker, .deb, .rpm, AUR, Snap, source |
| 3 | [Quick Start](user/03-quick-start.md) | Get running in under a minute |
| 4 | [Configuration](user/04-configuration.md) | Full `config.yaml` reference and environment variables |
| 5 | [Web Dashboard](user/05-web-dashboard.md) | Using the real-time web UI |
| 6 | [Terminal UI (TUI)](user/06-tui.md) | The `kula tui` terminal dashboard |
| 7 | [Authentication](user/07-authentication.md) | Passwords, sessions, and multi-user setup |
| 8 | [Application Monitoring](user/08-application-monitoring.md) | Nginx, Apache2, PostgreSQL, MySQL/MariaDB, containers |
| 9 | [Custom Metrics](user/09-custom-metrics.md) | Feed your own data through the Unix socket |
| 10 | [AI Assistant](user/10-ai-assistant.md) | Local Ollama-powered analysis |
| 11 | [Prometheus Exporter](user/11-prometheus.md) | Scraping Kula into observability stacks |
| 12 | [Backups](user/12-backups.md) | Scheduled snapshots of the storage tiers |
| 13 | [Reverse Proxy & TLS](user/13-reverse-proxy.md) | Running behind nginx/Apache, base paths, Unix sockets |
| 14 | [CLI Reference](user/14-cli-reference.md) | Every command and flag |
| 15 | [Service Management](user/15-service-management.md) | systemd, OpenRC, runit |
| 16 | [Troubleshooting](user/16-troubleshooting.md) | Common problems and fixes |

## 🛠️ Developer Guide

For contributors and people building on top of Kula.

| # | Document | Description |
|---|----------|-------------|
| 1 | [Architecture Overview](dev/01-architecture.md) | High-level design and data flow |
| 2 | [Project Layout](dev/02-project-layout.md) | Directory and file map |
| 3 | [Building & Toolchain](dev/03-building.md) | Build, cross-compile, dev workflow |
| 4 | [Collector Subsystem](dev/04-collector.md) | How metrics are gathered |
| 5 | [Storage Engine](dev/05-storage-engine.md) | Tiered ring-buffer storage |
| 6 | [Binary Codec](dev/06-storage-codec.md) | The on-disk record format |
| 7 | [Web Server & API](dev/07-web-server-api.md) | HTTP server, routes, and REST API |
| 8 | [Security Model](dev/08-security.md) | Auth, CSRF, headers, hardening |
| 9 | [Landlock Sandbox](dev/09-sandbox.md) | Filesystem & network confinement |
| 10 | [Frontend](dev/10-frontend.md) | The embedded SPA dashboard |
| 11 | [Internationalization](dev/11-i18n.md) | The i18n system and locales |
| 12 | [Testing & QA](dev/12-testing.md) | Unit, race, fuzz, and benchmark suites |
| 13 | [kula-scan](dev/13-kula-scan.md) | The black-box security scanner |
| 14 | [Adding a Metric Type](dev/14-adding-metrics.md) | Step-by-step codec extension guide |
| 15 | [Packaging & Release](dev/15-packaging.md) | Building distro packages and releases |
| 16 | [Contributing](dev/16-contributing.md) | Conventions, governance, and workflow |

---

## Quick Links

- **Website:** https://kula.ovh
- **Live demo:** https://demo.kula.ovh/
- **Docker Hub:** https://hub.docker.com/r/c0m4r/kula
- **Source:** https://github.com/c0m4r/kula
- **License:** [GNU AGPLv3](../LICENSE)
- **Default dashboard:** http://localhost:27960

> **Note on this documentation.** This `docs/` tree documents the codebase at version `0.20.0`.
> Where exact byte layouts, flag bits, or limits are quoted, they reflect the source as
> reviewed; always treat the code as the source of truth, and the
> project [wiki](https://github.com/c0m4r/kula/wiki) for the most current operational guides.
