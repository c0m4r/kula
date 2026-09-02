#!/usr/bin/env bash

set -euo pipefail

readonly GITHUB_REPO="c0m4r/kula"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
readonly DEST_DIR="${SCRIPT_DIR}/roles/kula/files"
readonly LATEST_RELEASE_URL="https://github.com/${GITHUB_REPO}/releases/latest"

die() {
    printf 'Error: %s\n' "$*" >&2
    exit 1
}

# Download a URL to a file with curl or wget; non-zero on any failure.
fetch() {
    local url=$1
    local output=$2

    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$url" -o "$output"
    elif command -v wget >/dev/null 2>&1; then
        wget -qO "$output" "$url"
    else
        die "neither curl nor wget is installed"
    fi
}

latest_version() {
    local headers
    local location
    local version

    if command -v curl >/dev/null 2>&1; then
        headers=$(curl -fsSI "$LATEST_RELEASE_URL") || \
            die "failed to resolve the latest GitHub release"
    elif command -v wget >/dev/null 2>&1; then
        # wget returns a failure status when redirects are deliberately disabled.
        headers=$(wget --server-response --spider --max-redirect=0 \
            "$LATEST_RELEASE_URL" 2>&1 || true)
    else
        die "neither curl nor wget is installed"
    fi

    location=$(awk '
        tolower($1) == "location:" { latest = $2 }
        END { sub(/\r$/, "", latest); print latest }
    ' <<<"$headers")
    version=${location##*/}

    [[ -n "$version" ]] || die "failed to determine the latest release version"
    [[ "$version" =~ ^[a-zA-Z0-9.-]+$ ]] || \
        die "invalid version returned by GitHub: $version"

    printf '%s\n' "$version"
}

command -v sha256sum >/dev/null 2>&1 || \
    die "sha256sum is required to verify release assets"

KULA_VERSION=$(latest_version)
readonly KULA_VERSION
readonly RELEASE_URL="https://github.com/${GITHUB_REPO}/releases/download/${KULA_VERSION}"
readonly DEB_ASSET="kula-${KULA_VERSION}-amd64.deb"
readonly RPM_ASSET="kula-${KULA_VERSION}-x86_64.rpm"

umask 077
TMP_DIR=$(mktemp -d /tmp/kula-ansible-XXXXXX)
readonly TMP_DIR
trap 'rm -rf -- "$TMP_DIR"' EXIT

readonly CHECKSUMS_FILE="${TMP_DIR}/CHECKSUMS.sha256.txt"

printf 'Fetching checksums for kula %s...\n' "$KULA_VERSION"
fetch "${RELEASE_URL}/CHECKSUMS.sha256.txt" "$CHECKSUMS_FILE"
[[ -s "$CHECKSUMS_FILE" ]] || \
    die "downloaded CHECKSUMS.sha256.txt is empty"

download_and_verify() {
    local filename=$1
    local target="${TMP_DIR}/${filename}"
    local expected
    local actual

    printf 'Downloading %s...\n' "$filename"
    fetch "${RELEASE_URL}/${filename}" "$target"
    [[ -s "$target" ]] || die "downloaded asset is empty: $filename"

    expected=$(awk -v filename="$filename" '$2 == filename { print $1 }' \
        "$CHECKSUMS_FILE")
    [[ -n "$expected" && "$expected" != *$'\n'* ]] || \
        die "expected exactly one checksum entry for $filename"
    [[ "$expected" =~ ^[[:xdigit:]]{64}$ ]] || \
        die "invalid checksum entry for $filename"

    actual=$(sha256sum "$target" | awk '{ print $1 }')
    if [[ "${expected,,}" != "${actual,,}" ]]; then
        printf 'Error: checksum mismatch for %s\n' "$filename" >&2
        printf '  expected: %s\n' "$expected" >&2
        printf '  actual:   %s\n' "$actual" >&2
        exit 1
    fi

    printf 'Verified %s (sha256: %s).\n' "$filename" "$actual"
}

# Verify both assets before replacing either file used by the Ansible role.
download_and_verify "$DEB_ASSET"
download_and_verify "$RPM_ASSET"

mkdir -p "$DEST_DIR"
install -m 0644 "${TMP_DIR}/${DEB_ASSET}" "${DEST_DIR}/kula.deb"
install -m 0644 "${TMP_DIR}/${RPM_ASSET}" "${DEST_DIR}/kula.rpm"

printf 'Populated %s with kula %s Debian and RPM packages.\n' \
    "$DEST_DIR" "$KULA_VERSION"
