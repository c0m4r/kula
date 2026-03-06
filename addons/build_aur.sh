#!/usr/bin/env bash

set -e

# Read version from VERSION file
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
VERSION_FILE="${PROJECT_ROOT}/VERSION"

if [ -f "${VERSION_FILE}" ]; then
    VERSION="$(head -1 "${VERSION_FILE}" | tr -d '[:space:]')"
else
    VERSION="0.5.0"
    echo "Warning: VERSION file not found, using default ${VERSION}"
fi

PKG_NAME="kula"
AUR_DIR="dist/aur"

# Choose between local and remote installation
echo "Select installation source:"
echo "  1) Local build (from current source checkout)"
echo "  2) Remote (GitHub release tarball) - NOT IMPLEMENTED YET"
read -rp "Choice [1]: " SOURCE_CHOICE
SOURCE_CHOICE="${SOURCE_CHOICE:-1}"

echo "Creating AUR directory structure..."
mkdir -p "${AUR_DIR}"

### TODO: add checksum verification

if [ "${SOURCE_CHOICE}" = "2" ]; then
    GITHUB_URL="https://github.com/c0m4r/kula"
    cat << EOF > "${AUR_DIR}/PKGBUILD"
# Maintainer: c0m4r
pkgname=${PKG_NAME}
pkgver=${VERSION}
pkgrel=1
pkgdesc="Lightweight system monitoring daemon."
arch=('x86_64' 'aarch64' 'riscv64')
url="${GITHUB_URL}"
license=('AGPL-3.0-or-later')
depends=('glibc')
makedepends=('go')
source=("\${pkgname}-\${pkgver}.tar.gz::${GITHUB_URL}/archive/v\${pkgver}.tar.gz")
sha256sums=('SKIP')
install='kula.install'

build() {
  cd "\${pkgname}-\${pkgver}"
  export CGO_ENABLED=0
  go build -o kula ./cmd/kula/
}

package() {
  cd "\${pkgname}-\${pkgver}"

  # Install binary
  install -Dm755 kula "\$pkgdir/usr/bin/kula"

  # Install systemd service
  install -Dm644 addons/init/systemd/kula.service "\$pkgdir/usr/lib/systemd/system/kula.service"

  # Install example config
  install -Dm644 config.example.yaml "\$pkgdir/etc/kula/config.example.yaml"

  # Create data directory
  install -dm755 "\$pkgdir/var/lib/kula"

  # Install bash completion
  install -Dm644 addons/bash-completion/kula "\$pkgdir/usr/share/bash-completion/completions/kula"

  # Install man page
  install -Dm644 addons/kula.1 "\$pkgdir/usr/share/man/man1/kula.1"

  # Install documentation
  for f in CHANGELOG VERSION README.md SECURITY.md LICENSE config.example.yaml; do
      if [ -f "\$f" ]; then
          install -Dm644 "\$f" "\$pkgdir/usr/share/kula/\$f"
      fi
  done
}
EOF
else
    cat << 'EOF' > "${AUR_DIR}/PKGBUILD"
# Maintainer: c0m4r
pkgname=kula
pkgver=VERSION_PLACEHOLDER
pkgrel=1
pkgdesc="Lightweight system monitoring daemon."
arch=('x86_64' 'aarch64' 'riscv64')
url="https://github.com/c0m4r/kula"
license=('AGPL-3.0-or-later')
depends=('glibc')
makedepends=('go')
# Local build from source checkout
source=()
sha256sums=()
install='kula.install'

build() {
  cd "$srcdir/../../.." # Go back to repo root from srcdir
  export CGO_ENABLED=0
  go build -o kula ./cmd/kula/
}

package() {
  cd "$srcdir/../../.."

  # Install binary
  install -Dm755 kula "$pkgdir/usr/bin/kula"
  
  # Install systemd service
  install -Dm644 addons/init/systemd/kula.service "$pkgdir/usr/lib/systemd/system/kula.service"

  # Install example config
  install -Dm644 config.example.yaml "$pkgdir/etc/kula/config.example.yaml"
  
  # Create data directory
  install -dm755 "$pkgdir/var/lib/kula"
  
  # Install bash completion
  install -Dm644 addons/bash-completion/kula "$pkgdir/usr/share/bash-completion/completions/kula"

  # Install man page
  install -Dm644 addons/kula.1 "$pkgdir/usr/share/man/man1/kula.1"

  # Install documentation
  for f in CHANGELOG VERSION README.md SECURITY.md LICENSE config.example.yaml; do
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
        useradd --system -g kula -d /var/lib/kula -s /bin/false -c "Kula System Monitoring Daemon" kula
    fi

    # Set ownership for directories the program will use
    chown -R kula:kula /etc/kula
    chown -R kula:kula /var/lib/kula

    # Load systemd, enable and start service
    if command -v systemctl >/dev/null; then
        systemctl daemon-reload || true
        systemctl enable kula.service || true
        systemctl start kula.service || true
    fi

    echo "Kula installed successfully!"
    echo "Default configuration is at /etc/kula/config.example.yaml"
    echo "To get started:"
    echo "  cp /etc/kula/config.example.yaml /etc/kula/config.yaml"
    echo "  kula serve"
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

echo "AUR package files generated in ${AUR_DIR}/"
echo "To build, cd ${AUR_DIR} and run 'makepkg -si'"
