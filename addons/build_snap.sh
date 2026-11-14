#!/usr/bin/env bash
#
# Build the Kula snap (snap/snapcraft.yaml) and drop it into dist/.
#
#   ./addons/build_snap.sh                   Build for the host architecture
#   ./addons/build_snap.sh cross|all         Cross-build amd64, arm64 and riscv64
#   ./addons/build_snap.sh --docker [cross]  Build in Docker, never in LXD
#   ./addons/build_snap.sh --lxd [cross]     Build in snapcraft's LXD instances
#   ./addons/build_snap.sh --remote          Build all architectures via Launchpad
#   ./addons/build_snap.sh -h|--help         Show this help
#
# The snap is built from source with the `go` plugin. Because kula is a pure-Go
# static binary with no stage-packages, every target can be cross-compiled in a
# host-arch build instance (the go plugin honours CRAFT_ARCH_BUILD_FOR), so a
# single amd64 box can produce all three .snap files — no emulation needed.
#
# Local builds run in Docker when it is usable (installed, its daemon reachable,
# an amd64 or arm64 host), else in LXD, and say which and why. A failed Docker
# build fails; it is never retried in LXD.
#
# LXD builds reach the network through an LXD bridge, which the host routes
# through its FORWARD chain. Docker sets that chain's policy to DROP when it
# enables IP forwarding itself, and the build instance then has no network.
# Before an LXD build the script reads the policy (as root, or through
# `sudo -n`, which works with NOPASSWD or after `sudo -v`) and, when it drops
# the bridge's traffic, stops and prints the rules that let the bridge through.
# KULA_SNAP_SKIP_FORWARD_CHECK=1 skips the check.
#
# Docker builds use Canonical's snapcraft image (amd64 and arm64 hosts),
# extended with Go by addons/build_snap/Dockerfile. They need Docker, but
# neither snapcraft nor LXD. Each platform is packed in a fresh container from
# a copy of the working tree (tracked and untracked files, minus ignored ones),
# and only the .snap comes back out. Snapcraft skips build-snaps in a container,
# so a build that needs one the image does not provide is refused.

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${PROJECT_ROOT}"

ALL_PLATFORMS=(amd64 arm64 riscv64)

usage() {
    # The header comment block, up to the first line that is not a comment.
    sed -n '3,/^[^#]/p' "${SCRIPT_DIR}/build_snap.sh" | sed '$d' | sed 's/^# \{0,1\}//'
}

MODE=auto
CROSS=0
for arg in "$@"; do
    case "${arg}" in
        -h|--help|help) usage; exit 0 ;;
        --docker|--lxd|--remote)
            if [ "${MODE}" != auto ] && [ "${MODE}" != "${arg#--}" ]; then
                echo "Error: --docker, --lxd and --remote are mutually exclusive"
                exit 1
            fi
            MODE="${arg#--}"
            ;;
        cross|all) CROSS=1 ;;
        *)
            echo "Unknown option: ${arg}"
            echo "Try: ./addons/build_snap.sh -h"
            exit 1
            ;;
    esac
done

require_snapcraft() {
    if ! command -v snapcraft &>/dev/null; then
        echo "Error: snapcraft is not installed."
        echo "Install it (and a build backend) with:"
        echo "  sudo snap install snapcraft --classic"
        echo "  sudo snap install lxd && sudo lxd init --auto   # recommended backend"
        echo "Docs: https://documentation.ubuntu.com/snapcraft/stable/howto/setup/"
        echo "Or build in Docker, without either: ./addons/build_snap.sh --docker"
        exit 1
    fi
}

# Read version from VERSION file
VERSION_FILE="${PROJECT_ROOT}/VERSION"
if [ -f "${VERSION_FILE}" ]; then
    VERSION="$(head -1 "${VERSION_FILE}" | tr -d '[:space:]')"
else
    echo "Error: VERSION file not found"
    exit 1
fi

# Map the host machine to a snap/Debian architecture name.
host_arch() {
    case "$(uname -m)" in
        x86_64)          echo "amd64" ;;
        aarch64|arm64)   echo "arm64" ;;
        riscv64)         echo "riscv64" ;;
        *) echo "Unsupported host architecture: $(uname -m)" >&2; exit 1 ;;
    esac
}

mkdir -p dist

# Move freshly produced kula_<version>_<arch>.snap files into dist/, renamed to
# the hyphenated kula-<version>-<arch>.snap convention used by the other dist
# artifacts (and matched by release.sh's CHECKSUMS glob).
collect_snaps() {
    shopt -s nullglob
    local built=( kula_*.snap )
    if [ ${#built[@]} -eq 0 ]; then
        echo "Error: no .snap artifact was produced"
        exit 1
    fi
    local snap arch dest
    for snap in "${built[@]}"; do
        arch="${snap##*_}"
        arch="${arch%.snap}"
        dest="dist/kula-${VERSION}-${arch}.snap"
        mv -f "${snap}" "${dest}"
        echo "Package built: ${dest}"
    done
}

# Remove orphaned craft-state mounts left on snapcraft's managed LXD instances
# by an interrupted build (crash, OOM kill, power loss). Snapcraft reuses these
# instances across runs and normally unmounts everything on exit, so any
# craft-state device still present before a build is orphaned. Its source is the
# previous run's state dir under XDG_RUNTIME_DIR, which vanishes on reboot; LXD
# then refuses to start the instance with "Missing source path ... for disk
# disk-/tmp/craft-state".
heal_stale_lxd_state_mounts() {
    command -v lxc &>/dev/null || return 0
    local inst device
    while IFS= read -r inst; do
        [[ "${inst}" == snapcraft-kula-* ]] || continue
        while IFS= read -r device; do
            [[ "${device}" == "disk-/tmp/craft-state" ]] || continue
            echo "Removing orphaned craft-state mount from LXD instance ${inst}"
            lxc config device remove --project snapcraft "${inst}" "${device}" >/dev/null
        done < <(lxc config device list --project snapcraft "${inst}" 2>/dev/null)
    done < <(lxc list --project snapcraft --format csv -c n 2>/dev/null)
}

# ── LXD network check ────────────────────────────────────────────────────────
# LXD's own accept rules for its bridge cannot override a DROP policy: with
# the nftables driver they live in a table of their own, and a packet must
# pass every base chain on the forward hook. The instance then resolves names
# (dnsmasq on the bridge answers from the host) but reaches nothing, and the
# build fails or stalls while fetching snaps and packages.

FORWARD_FIX=()         # commands that let the bridges through, one per line
FORWARD_UNVERIFIED=0   # 1 when Docker is present but the policy was unreadable
DOCKER_UNUSABLE=""     # why a default build fell back to LXD

# The bridges the NICs of snapcraft's LXD profile attach to. Snapcraft's LXD
# project copies the default project's profile when it creates it.
lxd_bridges() {
    local profile="" names name
    if command -v lxc &>/dev/null; then
        profile="$(lxc profile show default --project snapcraft 2>/dev/null \
            || lxc profile show default 2>/dev/null || true)"
    fi
    names="$(awk '$1 == "network:" || $1 == "parent:" { print $2 }' <<<"${profile}")"
    for name in ${names:-lxdbr0}; do
        # Only a bridge routes the instance through the FORWARD chain; a
        # macvlan parent does not.
        if [[ "${name}" =~ ^[A-Za-z0-9_.-]+$ ]] && [ -d "/sys/class/net/${name}/bridge" ]; then
            echo "${name}"
        fi
    done | sort -u
}

docker_present() {
    [ -e /sys/class/net/docker0 ] || [ -S /run/docker.sock ] || [ -S /var/run/docker.sock ]
}

# The filter table of TOOL (iptables or ip6tables) as `-S` lists it. Listing
# it needs root; a user gets it through sudo only without a password prompt.
filter_rules() { # TOOL
    if [ "${EUID}" -eq 0 ]; then
        "$1" -w 5 -S 2>/dev/null
    elif command -v sudo &>/dev/null; then
        sudo -n "$1" -w 5 -S 2>/dev/null
    else
        return 1
    fi
}

# Succeeds when RULES, an `-S` listing, drop what BRIDGE forwards: the FORWARD
# policy is DROP and no FORWARD or DOCKER-USER rule accepts the bridge's
# traffic both ways, out of it and back into it.
forward_blocks() { # RULES BRIDGE
    local br="${2//./\\.}"
    grep -qx -e '-P FORWARD DROP' <<<"$1" || return 1
    if grep -Eq -e "^-A (FORWARD|DOCKER-USER) (.+ )?-i ${br} (.+ )?-j ACCEPT( |$)" <<<"$1" \
        && grep -Eq -e "^-A (FORWARD|DOCKER-USER) (.+ )?-o ${br} (.+ )?-j ACCEPT( |$)" <<<"$1"; then
        return 1
    fi
    return 0
}

# The rules for BRIDGE go into DOCKER-USER when Docker owns the policy (Docker
# runs that chain first and leaves it alone when it restarts), else FORWARD.
add_forward_fix() { # TOOL CHAIN BRIDGE
    local tool
    tool="$(printf '%-9s' "$1")"
    FORWARD_FIX+=("sudo ${tool} -I $2 -i $3 -j ACCEPT")
    FORWARD_FIX+=("sudo ${tool} -I $2 -o $3 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT")
}

print_forward_fix() {
    local cross=""
    [ "${CROSS}" = 1 ] && cross=" cross"
    echo "Let the bridge through (the rules last until reboot; persist them the way"
    echo "your distribution saves iptables rules):"
    printf '  %s\n' "${FORWARD_FIX[@]}"
    if [ -n "${DOCKER_UNUSABLE}" ]; then
        echo "Docker could build instead, but ${DOCKER_UNUSABLE}."
    else
        echo "Or build in Docker, without LXD:"
        echo "  ./addons/build_snap.sh --docker${cross}"
    fi
}

check_lxd_forwarding() {
    local bridges rules chain br blocked=()
    [ "${KULA_SNAP_SKIP_FORWARD_CHECK:-0}" = 1 ] && return 0
    # Only snapcraft's default backend, LXD, builds behind the bridge.
    [ "${SNAPCRAFT_BUILD_ENVIRONMENT:-lxd}" = lxd ] || return 0
    bridges="$(lxd_bridges)"
    [ -n "${bridges}" ] || return 0
    # sudo logs every attempt that needs a password, so a user's sudo is only
    # tried where Docker makes the check worth it.
    if [ "${EUID}" -ne 0 ] && ! docker_present; then
        return 0
    fi

    if ! rules="$(filter_rules iptables)"; then
        docker_present || return 0
        FORWARD_UNVERIFIED=1
        for br in ${bridges}; do
            add_forward_fix iptables DOCKER-USER "${br}"
            add_forward_fix ip6tables DOCKER-USER "${br}"
        done
        echo "Note: Docker sets the FORWARD policy to DROP when it enables IP forwarding,"
        echo "which leaves snapcraft's LXD instance without network. Reading the policy"
        echo "needs root: run \`sudo -v\` first to let this script check it."
        return 0
    fi

    chain=FORWARD
    grep -qx -e '-N DOCKER-USER' <<<"${rules}" && chain=DOCKER-USER
    for br in ${bridges}; do
        if forward_blocks "${rules}" "${br}"; then
            add_forward_fix iptables "${chain}" "${br}"
            blocked+=("${br}")
        fi
    done
    [ ${#blocked[@]} -gt 0 ] || return 0

    # IPv6 rides along when its policy drops the same bridges.
    if rules="$(filter_rules ip6tables)"; then
        chain=FORWARD
        grep -qx -e '-N DOCKER-USER' <<<"${rules}" && chain=DOCKER-USER
        for br in "${blocked[@]}"; do
            if forward_blocks "${rules}" "${br}"; then
                add_forward_fix ip6tables "${chain}" "${br}"
            fi
        done
    fi

    echo "Error: the FORWARD chain's policy is DROP and no rule accepts the traffic of"
    echo "${blocked[*]}, so snapcraft's LXD instance would have no network. Docker sets"
    echo "this policy when it enables IP forwarding."
    print_forward_fix
    echo "Set KULA_SNAP_SKIP_FORWARD_CHECK=1 to build anyway."
    exit 1
}

lxd_pack() { # PLATFORM
    if ! snapcraft pack --platform "$1"; then
        if [ "${FORWARD_UNVERIFIED}" = 1 ]; then
            echo
            echo "The build failed. If it could not reach the network, Docker's FORWARD"
            echo "policy may be dropping the LXD bridge's traffic."
            print_forward_fix
        fi
        exit 1
    fi
}

# ── Docker builds ────────────────────────────────────────────────────────────

DOCKER_IMAGE="kula-snapcraft:core24"
CONTAINER=""

remove_container() {
    [ -z "${CONTAINER}" ] || docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
    CONTAINER=""
}
trap remove_container EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Why Docker cannot build the snap here, or nothing when it can.
docker_unusable_reason() {
    if ! command -v docker &>/dev/null; then
        echo "docker is not installed"
    elif ! docker info &>/dev/null; then
        echo "the Docker daemon cannot be reached"
    else
        case "$(docker version --format '{{.Server.Arch}}' 2>/dev/null)" in
            amd64|arm64) ;;
            *) echo "the snapcraft image exists for amd64 and arm64 Docker hosts only" ;;
        esac
    fi
}

# The image carries the Go release go.mod names in place of the `go` build
# snap, which snapcraft does not install inside a container. It is rebuilt
# when its Dockerfile or that version changes.
build_docker_image() {
    local go_version fingerprint
    go_version="$(sed -n 's/^go[[:space:]]\+\([0-9][^[:space:]]*\).*/\1/p' go.mod | head -n1)"
    if ! [[ "${go_version}" =~ ^[0-9]+(\.[0-9]+)*$ ]]; then
        echo "Error: cannot read the Go version from go.mod"
        exit 1
    fi
    fingerprint="$( { cat addons/build_snap/Dockerfile; echo "go=${go_version}"; } | sha256sum | cut -c1-16)"
    if [ "$(docker image inspect --format '{{ index .Config.Labels "kula.snapcraft.fingerprint" }}' \
            "${DOCKER_IMAGE}" 2>/dev/null)" = "${fingerprint}" ]; then
        return 0
    fi
    echo "Building the ${DOCKER_IMAGE} image (snapcraft and Go ${go_version})..."
    docker build \
        --file addons/build_snap/Dockerfile \
        --build-arg "GO_VERSION=${go_version}" \
        --label "kula.snapcraft.fingerprint=${fingerprint}" \
        --tag "${DOCKER_IMAGE}" \
        addons/build_snap
}

# The working tree as a tar stream: tracked and untracked files, minus ignored
# ones (dist/, built snaps, parts/, local data).
source_tar() {
    if git rev-parse --is-inside-work-tree &>/dev/null; then
        git ls-files -z --cached --others --exclude-standard | tar --null --files-from=- --create --file=-
    else
        tar --exclude=./.git --exclude=./dist --exclude=./data --exclude=./parts --exclude=./stage \
            --exclude=./prime --exclude='./*.snap' --create --file=- .
    fi
}

# Snapcraft skips build-snaps inside a container with a warning and carries
# on, so one the image does not provide would go missing unnoticed. The image
# is the core24 base itself and carries Go in place of the `go` snap; any other
# build snap stops the build. The warning always lists the base, so a finished
# build without it means the wording changed and the check no longer sees.
check_skipped_build_snaps() { # BUILD_STATUS
    local line snap missing=()
    line="$(docker logs "${CONTAINER}" 2>&1 \
        | grep -m1 -o 'running inside docker or podman container: .*' || true)"
    if [ -z "${line}" ]; then
        [ "$1" -ne 0 ] && return 0
        echo "Error: the container's log does not list the build snaps snapcraft skipped, so"
        echo "the build cannot be checked against the image. Either \`docker logs\` cannot read"
        echo "it, or the image's snapcraft words the warning differently: update"
        echo "check_skipped_build_snaps in addons/build_snap.sh."
        exit 1
    fi
    line="${line#*container: }"
    for snap in ${line//,/ }; do
        snap="${snap%.}"
        case "${snap%%/*}" in
            core24|go) ;;
            *) missing+=("${snap}") ;;
        esac
    done
    if [ ${#missing[@]} -gt 0 ]; then
        echo "Error: the build needs snaps the Docker image does not provide: ${missing[*]}."
        echo "Add them to addons/build_snap/Dockerfile, or build with --lxd."
        exit 1
    fi
}

# The image's entrypoint runs `snapcraft pack` in /project. The tree goes in
# and the snap comes out by `docker cp`, so nothing in the checkout is written
# as root and the daemon never needs to reach the checkout.
docker_pack() { # PLATFORM
    local snap="kula_${VERSION}_$1.snap" status=0
    CONTAINER="kula-snap-$1-$$"
    docker create --name "${CONTAINER}" "${DOCKER_IMAGE}" pack --platform "$1" >/dev/null
    source_tar | docker cp - "${CONTAINER}:/project"
    docker start --attach "${CONTAINER}" || status=$?
    check_skipped_build_snaps "${status}"
    [ "${status}" -eq 0 ] || exit "${status}"
    docker cp "${CONTAINER}:/project/${snap}" "${snap}"
    remove_container
}

# ── Build ────────────────────────────────────────────────────────────────────

if [ "${CROSS}" = 1 ]; then
    PLATFORMS=("${ALL_PLATFORMS[@]}")
elif [ "${MODE}" != remote ]; then
    arch="$(host_arch)"
    PLATFORMS=("${arch}")
fi

# Docker unless it cannot build here; --docker insists on it.
if [ "${MODE}" = auto ] || [ "${MODE}" = docker ]; then
    reason="$(docker_unusable_reason)"
    if [ -z "${reason}" ]; then
        [ "${MODE}" = auto ] && echo "Building in Docker (--lxd builds in LXD instead)."
        MODE=docker
    elif [ "${MODE}" = docker ]; then
        echo "Error: cannot build in Docker: ${reason}."
        exit 1
    else
        echo "Building in LXD: ${reason}."
        DOCKER_UNUSABLE="${reason}"
        MODE=lxd
    fi
fi

# Clear any stale artifacts so collect_snaps only sees this run's output.
rm -f kula_*.snap

case "${MODE}" in
    remote)
        require_snapcraft
        # Launchpad builds every platform declared in snapcraft.yaml.
        echo "Building Kula snap v${VERSION} for all architectures (Launchpad)..."
        snapcraft remote-build --launchpad-accept-public-upload
        collect_snaps
        ;;
    docker)
        build_docker_image
        if [ "${CROSS}" = 1 ]; then
            echo "Cross-building Kula snap v${VERSION} in Docker for: ${PLATFORMS[*]}"
        else
            echo "Building Kula snap v${VERSION} in Docker for host architecture (${PLATFORMS[0]})..."
        fi
        for p in "${PLATFORMS[@]}"; do
            [ "${CROSS}" = 1 ] && echo "==> ${p}"
            docker_pack "${p}"
            collect_snaps
        done
        ;;
    lxd)
        require_snapcraft
        check_lxd_forwarding
        # Local builds reuse snapcraft's LXD instances.
        heal_stale_lxd_state_mounts
        if [ "${CROSS}" = 1 ]; then
            echo "Cross-building Kula snap v${VERSION} for: ${PLATFORMS[*]}"
        else
            echo "Building Kula snap v${VERSION} for host architecture (${PLATFORMS[0]})..."
        fi
        for p in "${PLATFORMS[@]}"; do
            [ "${CROSS}" = 1 ] && echo "==> ${p}"
            lxd_pack "${p}"
            collect_snaps
        done
        ;;
esac
