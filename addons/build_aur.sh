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
AUR_DIR="dist/kula-$VERSION-aur"

# Choose between local and remote installation
echo "Select installation source:"
echo "  1) Local build (from current source checkout)"
echo "  2) Remote (GitHub release tarball)"
read -rp "Choice [1]: " SOURCE_CHOICE
SOURCE_CHOICE="${SOURCE_CHOICE:-1}"

echo "Creating AUR directory structure..."
mkdir -p "${AUR_DIR}"

### TODO: add checksum verification

if [ "${SOURCE_CHOICE}" = "2" ]; then
    echo "Using remote source (GitHub release tarball)"
    GITHUB_URL="https://github.com/c0m4r/kula"
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
sha256sums=('255de5051a917bf270f6c405c99d735329c590bdaaa9cafb1e4e89cd70118cc9')
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

  # Install example config
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

  # Install example config
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

    # Set ownership for directories the program will use
    chown -R kula:kula /etc/kula
    chown -R kula:kula /var/lib/kula

    # Reload systemd
    if command -v systemctl >/dev/null; then
        systemctl daemon-reload || true
    fi

    echo "Kula installed successfully!"
    echo "Default configuration is at /etc/kula/config.example.yaml"
    echo "To get started:"
    echo "  cp /etc/kula/config.example.yaml /etc/kula/config.yaml"
    echo "  systemctl enable --now kula.service"
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

cd "${AUR_DIR}"
makepkg --printsrcinfo > .SRCINFO
cat << EOF > .gitignore
*
!/.gitignore
!/kula.install
!/PKGBUILD
!/.SRCINFO
EOF

echo "AUR package files generated in ${AUR_DIR}/"
echo "To build, cd ${AUR_DIR} and run 'makepkg -si'"
