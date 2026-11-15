# Kula showcase

A Docker Compose stack that runs Kula with every feature on, against real services that
generate load, and a test that checks each feature end to end. Use it to see the full
dashboard, or to check a build before a release.

```bash
cd addons/docker/showcase
./showcase.sh up          # generate secrets and config, start, print URLs and passwords
./showcase.sh test        # check every feature (takes up to ~2 minutes: waits for a backup)
./showcase.sh down        # remove the stack, its volumes and the generated files
```

Needs Docker with Compose v2, rootful (the containers module reads `/var/run/docker.sock`
or the current Docker context's Unix socket). Nothing else on the host: the test runs in a
container.

| Option | Effect |
|---|---|
| `up --build` | Build Kula from this checkout as `kula:showcase` and run that instead of `c0m4r/kula:latest`. Later `up`s keep using it; edit `KULA_IMAGE` in `.env` to switch back. |
| `up --ai` | Also run Ollama and pull `qwen3:1.7b` (set `OLLAMA_MODEL` in `.env` for another model with tool calling). The Ollama image is 3.4 GB and the model 1.4 GB, and unpacking the image keeps the disk busy for a while. Answers run on the CPU, capped at half the host's CPUs (`OLLAMA_CPUS` in `.env`). Stays on until `up --no-ai`. |
| `info` | Print the URLs, passwords and token again. |
| `stop` | Stop the containers and keep their data; `up` starts them again. |

## What runs

Everything listens on `127.0.0.1` only.

| Port | Service | Kula feature |
|---|---|---|
| 27970 | Kula, at `/kula/` | Dashboard, API, WebSocket, `/metrics`, under `web.base_path` |
| 27971 | Caddy, HTTPS from its local CA | Reverse proxy: `web.trust_proxy` makes the session cookie `Secure` and adds HSTS |
| 27972 | Prometheus | Scrapes `/kula/metrics` with the bearer token |
| 27973 | nginx | `applications.nginx` (stub_status) |
| 27974 | Apache httpd | `applications.apache2` (mod_status, ExtendedStatus) |
| 27975 | PostgreSQL 18 | `applications.postgres`, as a `pg_monitor` role |
| 27976 | MariaDB 11.8 | `applications.mysql`, with the replication grants |
| 27977 | Ollama (`--ai`) | AI assistant, with `get_metrics` tool calling |

Kula runs as in the main [`docker-compose.yml`](../docker-compose.yml): host PID and network
namespaces, read-only, no capabilities, under its Landlock sandbox. On top of that it has:

- **Containers by name.** The Docker socket is mounted, with its group added to Kula. **Whoever
  can use that socket is root on the host**, so a Kula that is taken over takes the host with
  it; see [Application Monitoring](../../../docs/user/08-application-monitoring.md#containers-docker--podman).
  Without the socket, the containers module falls back to cgroups mode: the same metrics,
  under container IDs.
- **Logged-in users.** `showcase.sh` mounts whichever of `/run/utmp` and
  `/run/systemd/sessions` the host has, through `docker-compose.logins.yml`, which `.env`
  names in `COMPOSE_FILE`.
- **Custom metrics.** The `custom-metrics` service pushes two chart groups, `room` and
  `queue`, into `/app/data/kula.sock`, running as Kula's uid and gid.
- **Auth** with two accounts, `admin` and `viewer`, with random passwords.
- **Backups** every two minutes into `/app/data/backup`, kept 30 minutes, with smaller tiers
  than the default so they stay cheap.
- **Load.** `web-traffic` sends 4 to 30 requests a second to each web server and keeps idle
  keep-alive connections open; `pg-load` runs a rate-limited pgbench and now and then holds
  a transaction open; `mysql-load` runs inserts, selects, updates and deletes, and a slow
  query every half minute.

The terminal UI runs inside Kula's container:

```bash
docker compose exec kula /app/kula tui --config /app/config.yaml
```

## Generated files

`showcase.sh up` writes these, and `down` deletes them. All are ignored by git.

- `.env` (mode 0600): random passwords for the databases and the two Kula accounts, the
  `/metrics` token, Kula's password hashes, the Docker socket and its group, and the
  settings above.
- `config.yaml`: rendered from [`config.template.yaml`](config.template.yaml).
- `secrets/prometheus_token`, and `secrets/caddy_root.crt`, which the test uses to verify
  the proxy's certificate.

`config.yaml` and the token are world-readable, because Kula (uid 100) and Prometheus
(uid 65534) read them. That suits a local demo, not a shared host.

The databases keep the passwords they were initialized with, so `.env` stays until `down`
removes their volumes too. Edit `config.template.yaml` and run `up` again to change Kula's
config: `up` restarts Kula when `config.yaml` changes.

## The test

`./showcase.sh test` checks, against the running stack:

- the sandbox, the containers module's socket mode and the backup schedule, from Kula's log,
  and `kula inspect` over every tier;
- health under the base path, nothing outside it, login refusal and success for both
  accounts, `405` from read-only routes;
- `/api/current` with every application: nginx and Apache request rates, PostgreSQL
  connections, MariaDB queries, containers by name, both custom groups; the history, config,
  system info and translation APIs; a live WebSocket frame;
- through Caddy: health, then login, API and WebSocket, with a `Secure` cookie and HSTS;
- `/metrics` refusing a scrape without the token and serving every application's series
  with it, and Prometheus scraping it;
- the logged-in user count, a backup run with all three tiers;
- with `--ai`: the model list and a chat answer.

Each check prints `PASS`, `FAIL` or `SKIP`; the command exits 1 if any fails.

A run logs in once as `admin`, once as `viewer` and once with a wrong password under a third
name. Kula's login limiter allows five attempts per account and address in five minutes,
successful ones included, so up to five runs fit in that window; past it, logins answer
`429` and the test says to wait.
