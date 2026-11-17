#!/usr/bin/env bash
# Kula showcase: start, test and remove a stack with every Kula feature on.
# See README.md.
set -euo pipefail

cd "$(dirname "$0")"

usage() {
    cat <<'EOF'
Usage: ./showcase.sh COMMAND [OPTIONS]

  up [--ai|--no-ai] [--build]
                       generate secrets and config.yaml if needed, then start
                       --ai     also run Ollama and pull a model (about 5 GB
                                with its image); stays on until --no-ai
                       --no-ai  remove the Ollama containers, AI off
                       --build  build Kula from this checkout as kula:showcase
                                instead of running c0m4r/kula:latest
  test                 check every feature against the running stack, then
                       scan it with kula-scan built from this checkout
  info                 print the URLs and passwords
  stop                 stop the stack, keeping its data
  down                 remove the stack, its volumes and the generated files
EOF
}

die() {
    echo "showcase: $*" >&2
    exit 1
}

# secret BYTES: that many random bytes, in hex
secret() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }

get_env() { sed -n "s/^$1=//p" .env; }

set_env() {
    if grep -q "^$1=" .env; then
        sed -i "s|^$1=.*|$1=$2|" .env
    else
        printf '%s=%s\n' "$1" "$2" >>.env
    fi
}

# Secrets are generated once: the databases keep the passwords they were
# initialized with until ./showcase.sh down removes their volumes.
ensure_secrets() {
    if [[ ! -f .env ]]; then
        (umask 077 && : >.env)
        set_env KULA_IMAGE c0m4r/kula:latest
        set_env OLLAMA_MODEL qwen3:1.7b
        set_env SHOWCASE_AI 0
    fi
    local k
    for k in POSTGRES_PASSWORD KULA_POSTGRES_PASSWORD MARIADB_ROOT_PASSWORD MARIADB_PASSWORD \
        KULA_MYSQL_PASSWORD PROMETHEUS_TOKEN; do
        [[ -n "$(get_env "$k")" ]] || set_env "$k" "$(secret 24)"
    done
    for k in KULA_ADMIN_PASSWORD KULA_VIEWER_PASSWORD; do
        [[ -n "$(get_env "$k")" ]] || set_env "$k" "$(secret 8)"
    done
}

# hash_password USER: stores kula hash-password's hash and salt for USER
hash_password() {
    local user=$1 var out hash salt
    var=KULA_${user^^}
    [[ -n "$(get_env "${var}_HASH")" ]] && return
    out=$(printf '%s\n' "$(get_env "${var}_PASSWORD")" |
        docker run --rm -i --network none "$(get_env KULA_IMAGE)" hash-password) ||
        die "kula hash-password failed"
    hash=$(sed -n 's/^Password hash: //p' <<<"$out")
    salt=$(sed -n 's/^Salt: //p' <<<"$out")
    [[ -n "$hash" && -n "$salt" ]] || die "unexpected kula hash-password output: $out"
    set_env "${var}_HASH" "$hash"
    set_env "${var}_SALT" "$salt"
}

# The containers module names containers through the Docker socket.
detect_docker() {
    local host socket
    host=$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null || true)
    case "$host" in
    unix://*) socket=${host#unix://} ;;
    "") socket=/var/run/docker.sock ;;
    *) die "Docker answers at $host; the containers module needs its Unix socket" ;;
    esac
    [[ -S "$socket" ]] || die "no Docker socket at $socket"
    set_env DOCKER_SOCKET "$socket"
    set_env DOCKER_GID "$(stat -c %g "$socket")"
}

# The logged-in user count reads the host's utmp or systemd-logind sessions:
# mount whichever exists, through a second compose file named in .env.
detect_logins() {
    local found=() p
    for p in /run/utmp /run/systemd/sessions; do
        [[ -e "$p" ]] && found+=("$p")
    done
    if ((${#found[@]} == 0)); then
        rm -f docker-compose.logins.yml
        set_env COMPOSE_FILE docker-compose.yml
        set_env SHOWCASE_LOGINS 0
        return
    fi
    {
        echo "# Written by showcase.sh: the host's login records, for the logged-in user count."
        echo "services:"
        echo "  kula:"
        echo "    volumes:"
        for p in "${found[@]}"; do
            printf '      - type: bind\n        source: %s\n        target: %s\n        read_only: true\n' "$p" "$p"
        done
    } >docker-compose.logins.yml
    set_env COMPOSE_FILE docker-compose.yml:docker-compose.logins.yml
    set_env SHOWCASE_LOGINS 1
}

# config.yaml and the scrape token are read by Kula (uid 100) and Prometheus
# (uid 65534), so they are world-readable: a local demo only.
render() {
    local ai=false
    [[ "$(get_env SHOWCASE_AI)" == 1 ]] && ai=true
    sed -e "s|@PROMETHEUS_TOKEN@|$(get_env PROMETHEUS_TOKEN)|" \
        -e "s|@ADMIN_HASH@|$(get_env KULA_ADMIN_HASH)|" \
        -e "s|@ADMIN_SALT@|$(get_env KULA_ADMIN_SALT)|" \
        -e "s|@VIEWER_HASH@|$(get_env KULA_VIEWER_HASH)|" \
        -e "s|@VIEWER_SALT@|$(get_env KULA_VIEWER_SALT)|" \
        -e "s|@OLLAMA_ENABLED@|$ai|" \
        -e "s|@OLLAMA_MODEL@|$(get_env OLLAMA_MODEL)|" \
        config.template.yaml >config.yaml.new
    chmod 0644 config.yaml.new
    mv config.yaml.new config.yaml
    mkdir -p secrets
    chmod 0755 secrets
    printf '%s' "$(get_env PROMETHEUS_TOKEN)" >secrets/prometheus_token
    chmod 0644 secrets/prometheus_token
}

wait_healthy() {
    local id status i
    id=$(docker compose ps -q kula)
    for ((i = 0; i < 60; i++)); do
        status=$(docker inspect --format '{{.State.Health.Status}}' "$id")
        [[ "$status" == healthy ]] && return
        sleep 2
    done
    docker compose logs --tail 30 kula >&2
    die "Kula is not healthy"
}

cmd_up() {
    local ai=0 build=0 arg
    for arg in "$@"; do
        case "$arg" in
        --ai) ai=1 ;;
        --no-ai) ai=-1 ;;
        --build) build=1 ;;
        *) usage >&2 && exit 2 ;;
        esac
    done
    ensure_secrets
    if ((build)); then
        docker build -f ../Dockerfile -t kula:showcase ../../..
        set_env KULA_IMAGE kula:showcase
    fi
    if ((ai == 1)); then
        set_env SHOWCASE_AI 1
        set_env COMPOSE_PROFILES ai
        # Half the CPUs for the model, so answering does not stall the host
        set_env OLLAMA_CPUS "$(($(nproc) > 1 ? $(nproc) / 2 : 1))"
    elif ((ai == -1)); then
        docker compose --profile ai rm --stop --force ollama ollama-pull
        set_env SHOWCASE_AI 0
        sed -i '/^COMPOSE_PROFILES=/d' .env
    fi
    detect_docker
    detect_logins
    hash_password admin
    hash_password viewer
    local before=""
    [[ -f config.yaml ]] && before=$(sha256sum config.yaml)
    render
    docker compose up -d
    # Compose does not see a changed bind-mounted file: restart Kula so a
    # running one reads the new config.yaml.
    if [[ -n "$before" && "$before" != "$(sha256sum config.yaml)" ]]; then
        docker compose restart kula
    fi
    wait_healthy
    echo
    cmd_info
}

cmd_info() {
    [[ -f .env ]] || die "not set up; run ./showcase.sh up"
    cat <<EOF
Kula showcase ($(get_env KULA_IMAGE))

  Dashboard         http://127.0.0.1:27970/kula/
  Through Caddy     https://localhost:27971/kula/  (Caddy's local CA: the browser warns)
  Prometheus        http://127.0.0.1:27972/  (try: kula_custom_metric)
  Logins            admin / $(get_env KULA_ADMIN_PASSWORD)
                    viewer / $(get_env KULA_VIEWER_PASSWORD)
  /metrics token    $(get_env PROMETHEUS_TOKEN)
  Terminal UI       docker compose exec kula /app/kula tui --config /app/config.yaml
  Feature checks    ./showcase.sh test
EOF
    if [[ "$(get_env SHOWCASE_AI)" == 1 ]]; then
        echo "  AI model          $(get_env OLLAMA_MODEL)  (download: docker compose logs -f ollama-pull)"
    fi
}

check() {
    local name=$1
    shift
    if "$@" >/dev/null 2>&1; then
        echo "PASS $name"
    else
        echo "FAIL $name"
        failed=1
    fi
}

# Reads the whole log: grep -q would stop early, and pipefail would count the
# writer's SIGPIPE as a failure.
logged() { docker compose logs --no-log-prefix kula | grep -F "$1" >/dev/null; }

# scan NAME ARGS...: runs kula-scan with ARGS and a JSON report, keeping the
# report and the exit status in reports/NAME.json and reports/NAME.exit.
scan() {
    local name=$1 status=0
    shift
    echo "kula-scan $name: $*"
    docker compose --profile test run --rm -T scanner -format json "$@" \
        >"reports/$name.json" || status=$?
    echo "$status" >"reports/$name.exit"
    chmod 0644 "reports/$name.json" "reports/$name.exit"
}

cmd_test() {
    [[ -f .env ]] || die "not set up; run ./showcase.sh up"
    failed=0
    # Created here, before a container mounts it, so it is not root's.
    rm -rf reports && mkdir -m 0755 reports
    check "Landlock sandbox enforced" logged "Landlock sandbox enforced"
    check "containers module in socket mode" logged "monitoring enabled (mode: socket)"
    check "backup schedule logged" logged "Backup enabled"
    check "kula inspect reads every tier" \
        docker compose exec -T kula /app/kula inspect --config /app/config.yaml
    docker compose exec -T proxy cat /data/caddy/pki/authorities/local/root.crt \
        >secrets/caddy_root.crt || die "cannot read Caddy's root certificate"
    docker compose --profile test run --rm tester || failed=1

    # kula-scan from this checkout, after the feature checks: its -aggressive
    # probes lock login out from the address they come from for five minutes.
    # Through Caddy, Kula sees that address as 127.0.0.1, as for the feature
    # checks; the direct scan names another in X-Forwarded-For, which
    # web.trust_proxy honours, so each scan has its own login attempts. The
    # -dos-wait outlasts Kula's 30 s read timeout, and through Caddy its
    # header timeout as well.
    docker build -q -f scan/Dockerfile -t kula-scan:showcase ../../.. >/dev/null ||
        die "cannot build kula-scan"
    scan direct -username admin -aggressive -fuzz -dos-wait 35s \
        -H "X-Forwarded-For: 192.0.2.10" -ignore BYPASS-XFF \
        -expect auth,origin-validation,frame-protection,metrics-token \
        http://127.0.0.1:27970/kula/
    scan proxy -username admin -aggressive -fuzz -dos-wait 75s -insecure \
        -expect auth,hsts,origin-validation,frame-protection,metrics-token \
        https://localhost:27971/kula/
    docker compose --profile test run --rm tester python -u /test/scan_test.py || failed=1
    if ((failed)); then
        echo "Some checks failed."
        return 1
    fi
    echo "All checks passed."
}

cmd_down() {
    if [[ ! -f .env ]]; then
        # The compose file requires these; down needs no real values.
        export DOCKER_GID=0 KULA_POSTGRES_PASSWORD=- KULA_MYSQL_PASSWORD=- \
            POSTGRES_PASSWORD=- MARIADB_ROOT_PASSWORD=-
    fi
    docker compose --profile ai --profile test down --volumes --remove-orphans
    rm -rf .env config.yaml docker-compose.logins.yml secrets reports
}

case "${1:-}" in
up) shift && cmd_up "$@" ;;
test) cmd_test ;;
info) cmd_info ;;
stop) docker compose --profile ai stop ;;
down) cmd_down ;;
*) usage && exit 2 ;;
esac
