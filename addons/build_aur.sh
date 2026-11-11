#!/usr/bin/env bash

set -e

# Read version from VERSION file
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
VERSION_FILE="${PROJECT_ROOT}/VERSION"

if [ -f "${VERSION_FILE}" ]; then
    VERSION="$(head -1 "${VERSION_FILE}" | tr -d '[:space:]')"
else
    echo "Error: VERSION file not found"
    exit 1
fi

PKG_NAME="kula"
GITHUB_URL="https://github.com/c0m4r/kula"

# Operate from the project root so dist/ paths are consistent regardless of CWD
cd "${PROJECT_ROOT}"

AUR_DIR="dist/kula-$VERSION-aur"
ARCHIVE_FILE="dist/${VERSION}.tar.gz"
AUR_TARBALL="dist/kula-${VERSION}-aur.tar.gz"
CHECKSUMS="dist/CHECKSUMS.sha256.txt"
mkdir -p dist

# Download a file with curl or wget, failing hard on HTTP errors / empty output
download() {
    local url="$1" out="$2"
    if command -v curl >/dev/null; then
        curl -fSL "$url" -o "$out"
    elif command -v wget >/dev/null; then
        wget -O "$out" "$url"
    else
        echo "Error: neither curl nor wget is installed."
        return 1
    fi
}

# Add/replace an entry for a dist file in CHECKSUMS.sha256.txt (kept sorted by name)
append_checksum() {
    local f="$1" sum
    sum=$(cd dist && sha256sum "$f" | awk '{print $1}')
    if [ -f "${CHECKSUMS}" ]; then
        awk -v f="$f" '$2 != f' "${CHECKSUMS}" > "${CHECKSUMS}.tmp"
        mv "${CHECKSUMS}.tmp" "${CHECKSUMS}"
    fi
    echo "${sum}  ${f}" >> "${CHECKSUMS}"
    sort -k2 "${CHECKSUMS}" -o "${CHECKSUMS}"
}

# Choose between local and remote installation
echo "Select installation source:"
echo "  1) Local build (from current source checkout)"
echo "  2) Remote (GitHub release tarball)"
read -rp "Choice [1]: " SOURCE_CHOICE
SOURCE_CHOICE="${SOURCE_CHOICE:-1}"

echo "Creating AUR directory structure..."
mkdir -p "${AUR_DIR}"

# The AUR dir is regeneratable build output: if we bail out before completing
# (e.g. the release tarball is not published yet, or makepkg fails), don't leave
# a half-built dir behind. Disarmed on success just before the final messages.
trap 'rm -rf "${AUR_DIR}"' EXIT

if [ "${SOURCE_CHOICE}" = "2" ]; then
    echo "Using remote source (GitHub release tarball)"

    # The PKGBUILD pins the GitHub source archive, which only exists once the
    # release/tag is published. Fetch it and derive the real sha256 — failing
    # hard if it is not yet available.
    ARCHIVE_URL="${GITHUB_URL}/archive/${VERSION}.tar.gz"
    echo "Fetching source archive: ${ARCHIVE_URL}"
    if ! download "${ARCHIVE_URL}" "${ARCHIVE_FILE}" || [ ! -s "${ARCHIVE_FILE}" ]; then
        echo "Error: could not download ${ARCHIVE_URL}"
        echo "Publish the GitHub release for ${VERSION} first, then re-run this script."
        rm -f "${ARCHIVE_FILE}"
        exit 1
    fi
    ARCHIVE_SHA256=$(sha256sum "${ARCHIVE_FILE}" | awk '{print $1}')
    echo "Source archive sha256: ${ARCHIVE_SHA256}"

    cat << EOF > "${AUR_DIR}/PKGBUILD"
# Maintainer: c0m4r <https://github.com/c0m4r>
pkgname=${PKG_NAME}
pkgver=${VERSION}
pkgrel=1
pkgdesc="Lightweight, self-contained monitoring tool"
arch=('x86_64')
url="${GITHUB_URL}"
license=('AGPL-3.0')
depends=('glibc')
makedepends=('go')
source=("\${pkgname}-\${pkgver}.tar.gz::${GITHUB_URL}/archive/\${pkgver}.tar.gz")
sha256sums=('${ARCHIVE_SHA256}')
install='kula.install'

check() {
  cd "\${pkgname}-\${pkgver}"
  export CGO_ENABLED=1
  go vet ./...
  go test -v -race -skip TestLandlockEnforcement ./...
}

build() {
  cd "\${pkgname}-\${pkgver}"
  export CGO_ENABLED=0
  go build \
    -trimpath \
    -ldflags="-s -w" \
    -buildvcs=false \
    -o kula ./cmd/kula/
}

package() {
  cd "\${pkgname}-\${pkgver}"

  # Install binary
  install -Dm755 kula "\$pkgdir/usr/bin/kula"

  # Install systemd service
  install -Dm644 addons/init/systemd/kula.service "\$pkgdir/usr/lib/systemd/system/kula.service"

  # Install example config (post_install gives /etc/kula to group kula)
  install -dm750 "\$pkgdir/etc/kula"
  install -Dm640 config.example.yaml "\$pkgdir/etc/kula/config.example.yaml"

  # Create data directory
  install -dm750 "\$pkgdir/var/lib/kula"

  # Install bash completion
  install -Dm644 addons/bash-completion/kula "\$pkgdir/usr/share/bash-completion/completions/kula"

  # Create man directory
  install -dm755 "\$pkgdir/usr/share/man/man1"

  # Install man page
  if [ -f "addons/man/kula.1" ]; then
      install -Dm644 addons/man/kula.1 "\$pkgdir/usr/share/man/man1/kula.1"
  else
      install -Dm644 addons/kula.1 "\$pkgdir/usr/share/man/man1/kula.1"
  fi

  # Copy scripts directory
  if [ -d "scripts" ]; then
      cp -r scripts "\$pkgdir/usr/share/kula/"
  fi

  # Install documentation
  for f in CHANGELOG.md VERSION README.md SECURITY.md LICENSE config.example.yaml; do
      if [ -f "\$f" ]; then
          install -Dm644 "\$f" "\$pkgdir/usr/share/kula/\$f"
      fi
  done
}
EOF
else
    echo "Using local source (current source checkout)"
    cat << 'EOF' > "${AUR_DIR}/PKGBUILD"
# Maintainer: c0m4r <https://github.com/c0m4r>
pkgname=kula
pkgver=VERSION_PLACEHOLDER
pkgrel=1
pkgdesc="Lightweight, self-contained monitoring tool"
arch=('x86_64')
url="https://github.com/c0m4r/kula"
license=('AGPL-3.0')
depends=('glibc')
makedepends=('go')
# Local build from source checkout
source=()
sha256sums=()
install='kula.install'

build() {
  cd "$srcdir/../../.." # Go back to repo root from srcdir
  export CGO_ENABLED=0
  go build \
    -trimpath \
    -ldflags="-s -w" \
    -buildvcs=false \
    -o kula ./cmd/kula/
}

package() {
  cd "$srcdir/../../.."

  # Install binary
  install -Dm755 kula "$pkgdir/usr/bin/kula"
  
  # Install systemd service
  install -Dm644 addons/init/systemd/kula.service "$pkgdir/usr/lib/systemd/system/kula.service"

  # Install example config (post_install gives /etc/kula to group kula)
  install -dm750 "$pkgdir/etc/kula"
  install -Dm640 config.example.yaml "$pkgdir/etc/kula/config.example.yaml"
  
  # Create data directory
  install -dm750 "$pkgdir/var/lib/kula"
  
  # Install bash completion
  install -Dm644 addons/bash-completion/kula "$pkgdir/usr/share/bash-completion/completions/kula"

  # Create man directory
  install -dm755 "$pkgdir/usr/share/man/man1"

  # Install man page
  if [ -f "addons/man/kula.1" ]; then
      install -Dm644 addons/man/kula.1 "$pkgdir/usr/share/man/man1/kula.1"
  else
      install -Dm644 addons/kula.1 "$pkgdir/usr/share/man/man1/kula.1"
  fi

  # Copy scripts directory
  if [ -d "scripts" ]; then
      cp -r scripts "$pkgdir/usr/share/kula/"
  fi

  # Install documentation
  for f in CHANGELOG.md VERSION README.md SECURITY.md LICENSE config.example.yaml; do
      if [ -f "$f" ]; then
          install -Dm644 "$f" "$pkgdir/usr/share/kula/$f"
      fi
  done
}
EOF
    # Replace version placeholder
    sed -i "s/VERSION_PLACEHOLDER/${VERSION}/" "${AUR_DIR}/PKGBUILD"
fi

cat << 'EOF' > "${AUR_DIR}/kula.install"
post_install() {
    # Create kula group if it doesn't exist
    if ! getent group kula >/dev/null; then
        groupadd --system kula
    fi

    # Create kula user if it doesn't exist
    if ! getent passwd kula >/dev/null; then
        useradd --system -g kula -d /var/lib/kula -s /bin/false -c "Kula monitoring tool" kula
    fi

    # The config holds the password hash, tokens and database passwords:
    # root owns it and the daemon only reads it, through its group. Seed it
    # from the packaged example on first install. Older packages gave
    # /etc/kula to the daemon, so once the directory is root's (nothing can
    # swap its entries after that), tighten an existing config only if it is
    # a plain file with one link, never a link the daemon could have left.
    chown root:kula /etc/kula
    chmod 0750 /etc/kula
    if [ ! -e /etc/kula/config.yaml ] && [ ! -L /etc/kula/config.yaml ]; then
        install -m 0640 -o root -g kula /etc/kula/config.example.yaml /etc/kula/config.yaml
    elif [ -f /etc/kula/config.yaml ] && [ ! -L /etc/kula/config.yaml ] && [ "$(stat -c %h /etc/kula/config.yaml)" = 1 ]; then
        chown root:kula /etc/kula/config.yaml
        chmod 0640 /etc/kula/config.yaml
    else
        echo "kula: /etc/kula/config.yaml is a link or not a plain file; its owner and mode are left as they are" >&2
    fi

    # Storage belongs to the daemon
    chown -R kula:kula /var/lib/kula

    # Reload systemd
    if command -v systemctl >/dev/null; then
        systemctl daemon-reload || true
    fi

    echo "Kula installed successfully!"
    echo "An active config has been seeded at /etc/kula/config.yaml"
    echo "To get started: systemctl enable --now kula.service"
}

post_upgrade() {
    post_install
}

pre_remove() {
    if command -v systemctl >/dev/null; then
        systemctl stop kula.service || true
        systemctl disable kula.service || true
    fi
}
EOF

(
    cd "${AUR_DIR}"
    makepkg --printsrcinfo > .SRCINFO
    cat << EOF > .gitignore
*
!/.gitignore
!/kula.install
!/PKGBUILD
!/.SRCINFO
EOF
)

# Package the AUR files into the artifact the guided installer downloads
tar --owner=0 --group=0 --numeric-owner -czf "${AUR_TARBALL}" -C dist "kula-${VERSION}-aur"
echo "Created ${AUR_TARBALL}"

# Record checksums so the guided installer can verify the AUR path (remote builds only)
if [ "${SOURCE_CHOICE}" = "2" ]; then
    append_checksum "$(basename "${ARCHIVE_FILE}")"
    append_checksum "$(basename "${AUR_TARBALL}")"
    echo "Updated ${CHECKSUMS} — re-upload it to the GitHub release."
fi

# Completed successfully — keep the generated AUR dir.
trap - EXIT

echo "AUR package files generated in ${AUR_DIR}/"
echo "To build, cd ${AUR_DIR} and run 'makepkg -si'"
