#!/usr/bin/env bash
#
# Build the Kula snap (snap/snapcraft.yaml) and drop it into dist/.
#
#   ./addons/build_snap.sh             Build for the current host architecture
#   ./addons/build_snap.sh cross|all   Cross-build amd64, arm64 and riscv64 locally
#   ./addons/build_snap.sh --remote    Build all architectures via Launchpad
#   ./addons/build_snap.sh -h|--help   Show this help
#
# The snap is built from source with the `go` plugin. Because kula is a pure-Go
# static binary with no stage-packages, every target can be cross-compiled in a
# host-arch build instance (the go plugin honours CRAFT_ARCH_BUILD_FOR), so a
# single amd64 box can produce all three .snap files — no emulation needed.

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${PROJECT_ROOT}"

ARG="${1:-}"

ALL_PLATFORMS=(amd64 arm64 riscv64)

if [[ "${ARG}" == "-h" || "${ARG}" == "--help" || "${ARG}" == "help" ]]; then
    sed -n '3,13p' "${SCRIPT_DIR}/build_snap.sh" | sed 's/^# \{0,1\}//'
    exit 0
fi

# Check if snapcraft is installed
if ! command -v snapcraft &>/dev/null; then
    echo "Error: snapcraft is not installed."
    echo "Install it (and a build backend) with:"
    echo "  sudo snap install snapcraft --classic"
    echo "  sudo snap install lxd && sudo lxd init --auto   # recommended backend"
    echo "Docs: https://documentation.ubuntu.com/snapcraft/stable/howto/setup/"
    exit 1
fi

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

# Clear any stale artifacts so collect_snaps only sees this run's output.
rm -f kula_*.snap

# Local builds reuse snapcraft's LXD instances; remote builds run on Launchpad.
if [[ "${ARG}" != "--remote" ]]; then
    heal_stale_lxd_state_mounts
fi

case "${ARG}" in
    --remote)
        # Launchpad builds every platform declared in snapcraft.yaml.
        echo "Building Kula snap v${VERSION} for all architectures (Launchpad)..."
        snapcraft remote-build --launchpad-accept-public-upload
        collect_snaps
        ;;
    cross|all)
        echo "Cross-building Kula snap v${VERSION} for: ${ALL_PLATFORMS[*]}"
        for p in "${ALL_PLATFORMS[@]}"; do
            echo "==> ${p}"
            snapcraft pack --platform "${p}"
            collect_snaps
        done
        ;;
    "")
        arch="$(host_arch)"
        echo "Building Kula snap v${VERSION} for host architecture (${arch})..."
        snapcraft pack --platform "${arch}"
        collect_snaps
        ;;
    *)
        echo "Unknown option: ${ARG}"
        echo "Try: ./addons/build_snap.sh -h"
        exit 1
        ;;
esac
