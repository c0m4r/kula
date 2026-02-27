#!/usr/bin/env bash

set -e

# Read version from VERSION file
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
VERSION_FILE="${PROJECT_ROOT}/VERSION"

if [ -f "${VERSION_FILE}" ]; then
    VERSION="$(head -1 "${VERSION_FILE}" | tr -d '[:space:]')"
else
    echo "Warning: VERSION file not found, using default 0.1.0"
    VERSION="0.1.0"
fi

PKG_NAME="kula"
AUR_DIR="aur"

# Choose between local and remote installation
echo "Select installation source:"
echo "  1) Local build (from current source checkout)"
echo "  2) Remote (GitHub release tarball)"
read -rp "Choice [1]: " SOURCE_CHOICE
SOURCE_CHOICE="${SOURCE_CHOICE:-1}"

echo "Creating AUR directory structure..."
mkdir -p "${AUR_DIR}"

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

  # Install example config
  install -Dm644 config.example.yaml "\$pkgdir/etc/kula/config.example.yaml"

  # Create data directory
  install -dm755 "\$pkgdir/var/lib/kula"

  # Install bash completion
  install -Dm644 docs/kula-completion.bash "\$pkgdir/usr/share/bash-completion/completions/kula"

  # Install man page
  install -Dm644 docs/kula.1 "\$pkgdir/usr/share/man/man1/kula.1"
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
  cd "$srcdir/../../" # Go back to repo root from srcdir
  export CGO_ENABLED=0
  go build -o kula ./cmd/kula/
}

package() {
  cd "$srcdir/../../"

  # Install binary
  install -Dm755 kula "$pkgdir/usr/bin/kula"
  
  # Install example config
  install -Dm644 config.example.yaml "$pkgdir/etc/kula/config.example.yaml"
  
  # Create data directory
  install -dm755 "$pkgdir/var/lib/kula"
  
  # Install bash completion
  install -Dm644 docs/kula-completion.bash "$pkgdir/usr/share/bash-completion/completions/kula"

  # Install man page
  install -Dm644 docs/kula.1 "$pkgdir/usr/share/man/man1/kula.1"
}
EOF
    # Replace version placeholder
    sed -i "s/VERSION_PLACEHOLDER/${VERSION}/" "${AUR_DIR}/PKGBUILD"
fi

cat << 'EOF' > "${AUR_DIR}/kula.install"
post_install() {
    echo "Kula installed successfully!"
    echo "Default configuration is at /etc/kula/config.example.yaml"
    echo "To get started:"
    echo "  cp /etc/kula/config.example.yaml /etc/kula/config.yaml"
    echo "  kula serve"
}
EOF

echo "AUR package files generated in ${AUR_DIR}/"
echo "To build, cd ${AUR_DIR} and run 'makepkg -si'"
