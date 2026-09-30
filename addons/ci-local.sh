#!/usr/bin/env bash
# Run the GitHub Actions workflows of this repository locally, in a container
# that stands in for the ubuntu-latest hosted runner: same Ubuntu release, same
# non-root runner user with passwordless sudo, Chrome, and a toolcache holding
# the Go, Node.js and Python versions the workflows ask for. The jobs are
# executed by addons/ci-local/run-workflow.py, which understands the step
# subset the workflows use and fails loudly on anything it does not emulate.
#
#   ./addons/ci-local.sh                    list workflows and their jobs
#   ./addons/ci-local.sh list               the same
#   ./addons/ci-local.sh run ci             run every job of .github/workflows/ci.yml
#   ./addons/ci-local.sh run ci ci          run job "ci" of ci.yml
#   ./addons/ci-local.sh run-all            every job of every workflow
#   ./addons/ci-local.sh shell              interactive shell in the runner image
#   ./addons/ci-local.sh build              build or refresh the runner image
#   ./addons/ci-local.sh clean              drop the image and the cache volumes
#
# Options (before or after the command):
#   --rebuild          rebuild the image, re-resolving the toolcache versions
#   --pull             refresh the Ubuntu/Chrome layers from the registry
#   --clean-cache      drop the Go/npm/pip cache volumes before running
#   --step PATTERN     run only the `run` steps whose name contains PATTERN
#                      (repeatable); setup actions always run
#   -e KEY=VALUE       extra environment variable for the container (repeatable)
#
# The working tree (tracked plus untracked, minus ignored files) is snapshotted
# into the container and committed there, so a job sees the code as it is right
# now. Each run writes its own snapshot into .ci-local/ (self-ignoring and
# private to you) because the Docker daemon must be able to read the
# bind-mounted path, and removes it on exit, Ctrl-C included. Caches
# live in named volumes, so a second run does not re-download the Go module
# cache or rebuild the world; --clean-cache or `clean` starts over. The
# container gets a 1 GiB /dev/shm, since Docker's 64 MiB default makes Chrome
# crawl and the browser fixtures measure the difference.
#
# This is an equivalent, not a simulator: the security model of the workflows
# (read-only token, no persisted credentials) holds trivially because there is
# no token at all, and steps that need one (a push, a release upload) cannot
# work here by design.

set -euo pipefail

GREEN="\033[0;32m"
CYAN="\033[0;36m"
RED="\033[0;31m"
YELLOW="\033[0;33m"
RESET="\033[0m"

cd "$(dirname "$0")/.."

UBUNTU_VERSION="${CI_LOCAL_UBUNTU:-24.04}"
IMAGE_BASE="kula-ci-local"
VOLUME_PREFIX="kula-ci-local"
# The snapshot lives next to the checkout, not in /tmp: the Docker daemon must
# be able to read the bind-mounted path (it cannot see a private /tmp, and
# Docker Desktop only shares the user's own directories anyway).
SNAPSHOT_DIR="$PWD/.ci-local"

usage() {
    # The header comment block, up to the first line that is not a comment.
    sed -n '2,/^[^#]/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
}

# ── workflow settings ────────────────────────────────────────────────────────
# The image is built for the versions the workflows request, so bumping
# `go-version-file`/`node-version`/`python-version` in a workflow is enough.

workflow_setting() { # KEY
    grep -rhoE "^[[:space:]]*$1:[[:space:]]*['\"]?[^'\"[:space:]#]+" .github/workflows 2>/dev/null \
        | head -n1 | sed -E "s/.*$1:[[:space:]]*['\"]?//" || true
}

GO_VERSION="$(workflow_setting go-version)"
if [ -z "$GO_VERSION" ]; then
    GO_VERSION="$(sed -n 's/^go[[:space:]]\+\([0-9][^[:space:]]*\).*/\1/p' go.mod | head -n1)"
fi
NODE_VERSION="$(workflow_setting node-version)"
PYTHON_VERSION="$(workflow_setting python-version)"
: "${NODE_VERSION:=22}"
: "${PYTHON_VERSION:=3.x}"

if [ -z "$GO_VERSION" ]; then
    echo -e "${RED}ci-local: cannot determine the Go version from go.mod or the workflows${RESET}" >&2
    exit 1
fi

IMAGE_TAG="${IMAGE_BASE}:ubuntu${UBUNTU_VERSION}-go${GO_VERSION}-node${NODE_VERSION}-py${PYTHON_VERSION}"
IMAGE_TAG="$(printf '%s' "$IMAGE_TAG" | tr -c 'A-Za-z0-9_.:' '-')"

VOLUMES=(
    "${VOLUME_PREFIX}-gopath:/home/runner/go"
    "${VOLUME_PREFIX}-cache:/home/runner/.cache"
    "${VOLUME_PREFIX}-npm:/home/runner/.npm"
)

fingerprint() {
    {
        cat addons/ci-local/Dockerfile addons/ci-local/entrypoint.sh addons/ci-local/run-workflow.py
        echo "ubuntu=$UBUNTU_VERSION go=$GO_VERSION node=$NODE_VERSION python=$PYTHON_VERSION"
    } | sha256sum | cut -c1-16
}

image_fingerprint() {
    docker image inspect --format '{{ index .Config.Labels "ci-local.fingerprint" }}' "$1" 2>/dev/null || true
}

build_image() {
    local want
    want="$(fingerprint)"
    if [ "$FORCE_REBUILD" != "1" ] && [ "$(image_fingerprint "$IMAGE_TAG")" = "$want" ]; then
        return 0
    fi
    echo -e "${CYAN}Building $IMAGE_TAG${RESET}"
    echo "  Ubuntu $UBUNTU_VERSION, Go $GO_VERSION, Node.js $NODE_VERSION, Python $PYTHON_VERSION"
    local extra=()
    [ "$FORCE_REBUILD" = "1" ] && extra+=(--no-cache)
    [ "$PULL" = "1" ] && extra+=(--pull)
    docker build \
        --file addons/ci-local/Dockerfile \
        --build-arg "UBUNTU_VERSION=$UBUNTU_VERSION" \
        --build-arg "GO_VERSIONS=$GO_VERSION" \
        --build-arg "NODE_VERSIONS=$NODE_VERSION" \
        --build-arg "PYTHON_VERSIONS=$PYTHON_VERSION" \
        --label "ci-local.fingerprint=$want" \
        "${extra[@]+"${extra[@]}"}" \
        --tag "$IMAGE_TAG" \
        addons/ci-local
}

clean_caches() {
    for volume in "${VOLUMES[@]%%:*}"; do
        docker volume rm "$volume" >/dev/null 2>&1 || true
    done
}

require_docker() {
    if ! command -v docker >/dev/null 2>&1; then
        echo -e "${RED}ci-local: docker is not installed${RESET}" >&2
        exit 1
    fi
    if ! docker info >/dev/null 2>&1; then
        echo -e "${RED}ci-local: cannot talk to the docker daemon${RESET}" >&2
        exit 1
    fi
}

SNAPSHOT=""
remove_snapshot() {
    [ -z "$SNAPSHOT" ] || rm -f "$SNAPSHOT"
}
trap remove_snapshot EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

snapshot_tree() { # prints the snapshot path
    mkdir -p "$SNAPSHOT_DIR"
    # The snapshot holds untracked files too; only its owner may reach it.
    chmod 0700 "$SNAPSHOT_DIR"
    # The directory ignores itself, so the snapshot is never an input to itself.
    [ -f "$SNAPSHOT_DIR/.gitignore" ] || printf '*\n' > "$SNAPSHOT_DIR/.gitignore"
    # Left behind by versions that used one fixed, world-readable name.
    rm -f "$SNAPSHOT_DIR/src.tar" 2>/dev/null || true
    # A private name per run: concurrent runs must not share or delete it.
    local out
    out="$(mktemp "$SNAPSHOT_DIR/src.XXXXXXXX")"
    SNAPSHOT="$out"
    if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
        git ls-files -z --cached --others --exclude-standard | tar --null --files-from=- --create --file="$out"
    else
        tar --exclude=./.git --exclude=./.ci-local --create --file="$out" .
    fi
    # The container runs as uid 1001 and must read it; the 0700 directory
    # keeps other local users out.
    chmod 0644 "$out"
}

remote_url() {
    git config --get remote.origin.url 2>/dev/null || basename "$PWD"
}

ref_name() {
    local name
    name="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
    if [ -z "$name" ] || [ "$name" = "HEAD" ]; then
        name="$(git rev-parse --short HEAD 2>/dev/null || echo main)"
    fi
    echo "$name"
}

run_container() { # ARGS...
    local status=0 tty=()
    snapshot_tree
    if [ -t 0 ] && [ -t 1 ]; then
        tty+=(-t)
    fi

    local volumes=()
    for volume in "${VOLUMES[@]}"; do
        volumes+=(-v "$volume")
    done
    local env=(
        -e "CI_LOCAL=true"
        -e "CI_LOCAL_REPOSITORY=$(remote_url)"
        -e "CI_LOCAL_REF_NAME=$(ref_name)"
        -e "CI_LOCAL_SHA=$(git rev-parse HEAD 2>/dev/null || echo 0000000000000000000000000000000000000000)"
    )
    for item in "${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"}"; do
        env+=(-e "$item")
    done

    # --shm-size: Docker defaults /dev/shm to 64 MiB, which makes Chrome crawl;
    # the hosted runner has a normally sized /dev/shm, and the browser fixtures
    # notice the difference.
    # --mount, unlike -v, fails on a missing source instead of creating a
    # root-owned directory in its place.
    docker run --rm --init -i "${tty[@]+"${tty[@]}"}" \
        --shm-size=1g \
        --mount "type=bind,\"source=$SNAPSHOT\",target=/ci/src.tar,readonly" \
        "${volumes[@]}" \
        "${env[@]}" \
        "$IMAGE_TAG" "$@" || status=$?
    remove_snapshot
    SNAPSHOT=""
    return "$status"
}

# ── argument parsing ─────────────────────────────────────────────────────────

COMMAND=""
POSITIONAL=()
EXTRA_ENV=()
STEP_FILTERS=()
FORCE_REBUILD=0
PULL=0
CLEAN_CACHE=0

while [ $# -gt 0 ]; do
    case "$1" in
        -h|--help|help) usage; exit 0 ;;
        --rebuild) FORCE_REBUILD=1 ;;
        --pull) PULL=1 ;;
        --clean-cache) CLEAN_CACHE=1 ;;
        --step) shift; STEP_FILTERS+=("${1:?--step needs a pattern}") ;;
        --step=*) STEP_FILTERS+=("${1#--step=}") ;;
        -e) shift; EXTRA_ENV+=("${1:?-e needs KEY=VALUE}") ;;
        -e*) EXTRA_ENV+=("${1#-e}") ;;
        -*) echo -e "${RED}ci-local: unknown option $1${RESET}" >&2; usage >&2; exit 2 ;;
        *)
            if [ -z "$COMMAND" ]; then
                COMMAND="$1"
            else
                POSITIONAL+=("$1")
            fi
            ;;
    esac
    shift
done
: "${COMMAND:=list}"

case "$COMMAND" in
    list|run|run-all|shell|build|clean) ;;
    *) echo -e "${RED}ci-local: unknown command $COMMAND${RESET}" >&2; usage >&2; exit 2 ;;
esac

require_docker

if [ "$CLEAN_CACHE" = "1" ]; then
    echo -e "${YELLOW}Dropping the Go, npm and pip cache volumes${RESET}"
    clean_caches
fi

if [ "$COMMAND" = "build" ]; then
    build_image
    echo -e "${GREEN}Image $IMAGE_TAG is ready${RESET}"
    exit 0
fi

if [ "$COMMAND" = "clean" ]; then
    # Every ci-local tag, not just the current one: a toolchain bump leaves the
    # previous tag behind, and those images are large.
    for reference in $(docker image ls --format '{{.Repository}}:{{.Tag}}' | grep "^${IMAGE_BASE}:" || true); do
        docker image rm "$reference" >/dev/null 2>&1 || true
        echo -e "${GREEN}Removed image $reference${RESET}"
    done
    clean_caches
    echo -e "${GREEN}Removed the ${IMAGE_BASE} images and the cache volumes${RESET}"
    exit 0
fi

build_image

case "$COMMAND" in
    list)
        echo -e "${CYAN}Workflows in .github/workflows (runner: $IMAGE_TAG)${RESET}"
        run_container list "${POSITIONAL[@]+"${POSITIONAL[@]}"}"
        ;;
    run)
        if [ "${#POSITIONAL[@]}" -eq 0 ]; then
            echo -e "${RED}ci-local: run needs a workflow name${RESET}" >&2
            exit 2
        fi
        args=()
        for pattern in "${STEP_FILTERS[@]+"${STEP_FILTERS[@]}"}"; do
            args+=(--step "$pattern")
        done
        run_container "${args[@]+"${args[@]}"}" run "${POSITIONAL[@]}"
        ;;
    run-all)
        args=()
        for pattern in "${STEP_FILTERS[@]+"${STEP_FILTERS[@]}"}"; do
            args+=(--step "$pattern")
        done
        run_container "${args[@]+"${args[@]}"}" run-all
        ;;
    shell)
        echo -e "${CYAN}Opening a shell in $IMAGE_TAG (the workspace snapshot is discarded on exit)${RESET}"
        run_container shell
        ;;
esac
