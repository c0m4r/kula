#!/usr/bin/env bash

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd ${SCRIPT_DIR}/..

ARCH=$1

if [ -z "$ARCH" ]; then
    ARCH=$(cat /proc/sys/kernel/arch)
    case "$ARCH" in
        x86_64) ARCH="amd64" ;;
        aarch64) ARCH="arm64" ;;
        riscv64) ARCH="riscv64" ;;
        *) echo "Unsupported architecture: $ARCH" ; exit 1 ;;
    esac
else
    # only allow amd64,arm64,riscv64
    case "$ARCH" in
        amd64|arm64|riscv64) ;; # ok
        *) echo "Unsupported architecture: $ARCH" ; exit 1 ;;
    esac
fi

# Check if dpkg-deb is installed
if ! command -v dpkg-deb &>/dev/null; then
    echo "Error: dpkg-deb is not installed."
    echo "Install dpkg:"
    echo "  Arch Linux:    sudo pacman -S dpkg"
    echo "  Fedora:        sudo dnf install dpkg"
    echo "  Or visit:      https://git.dpkg.org/git/dpkg/dpkg.git/"
    exit 1
fi

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

# Configuration
PKG_NAME="kula"
MAINTAINER="c0m4r"
DESCRIPTION="Lightweight system monitoring daemon."
BUILD_DIR="build_deb"
PKG_DIR="${BUILD_DIR}/${PKG_NAME}_${VERSION}_${ARCH}"

# Check if binary exists, build if not
if [ ! -f "dist/kula-linux-${VERSION}-${ARCH}" ]; then
    echo "kula binary not found, building first..."
    ${SCRIPT_DIR}/build.sh cross
fi

echo "Building Debian package v${VERSION}..."

echo "Cleaning up old build directory..."
rm -rf "${BUILD_DIR}"

echo "Creating directory structure..."
mkdir -p "${PKG_DIR}/DEBIAN"
mkdir -p "${PKG_DIR}/usr/bin"
mkdir -p "${PKG_DIR}/etc/kula"
mkdir -p "${PKG_DIR}/var/lib/kula"
mkdir -p "${PKG_DIR}/usr/share/kula"
mkdir -p "${PKG_DIR}/usr/share/bash-completion/completions"
mkdir -p "${PKG_DIR}/usr/share/man/man1"
mkdir -p "${PKG_DIR}/lib/systemd/system"

echo "Copying files..."
cp dist/kula-linux-${VERSION}-${ARCH} "${PKG_DIR}/usr/bin/kula"
cp config.example.yaml "${PKG_DIR}/etc/kula/config.example.yaml"
cp addons/bash-completion/kula "${PKG_DIR}/usr/share/bash-completion/completions/kula"
cp addons/init/systemd/kula.service "${PKG_DIR}/lib/systemd/system/kula.service"

for f in CHANGELOG VERSION README.md SECURITY.md LICENSE config.example.yaml; do
    if [ -f "$f" ]; then
        cp "$f" "${PKG_DIR}/usr/share/kula/"
    fi
done

# Compress and copy man page
gzip -c addons/kula.1 > "${PKG_DIR}/usr/share/man/man1/kula.1.gz"

echo "Creating DEBIAN control file..."
cat <<EOF > "${PKG_DIR}/DEBIAN/control"
Package: ${PKG_NAME}
Version: ${VERSION}
Architecture: ${ARCH}
Maintainer: ${MAINTAINER}
Description: ${DESCRIPTION}
EOF

echo "Creating DEBIAN postinst file..."
cat <<EOF > "${PKG_DIR}/DEBIAN/postinst"
#!/bin/sh
set -e

if [ "\$1" = "configure" ]; then
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
fi

exit 0
EOF
chmod 755 "${PKG_DIR}/DEBIAN/postinst"

echo "Creating DEBIAN prerm file..."
cat <<EOF > "${PKG_DIR}/DEBIAN/prerm"
#!/bin/sh
set -e

if [ "\$1" = "remove" ] || [ "\$1" = "deconfigure" ]; then
    if command -v systemctl >/dev/null; then
        systemctl stop kula.service || true
        systemctl disable kula.service || true
    fi
fi

exit 0
EOF
chmod 755 "${PKG_DIR}/DEBIAN/prerm"

# Set proper permissions
chmod 755 "${PKG_DIR}/usr/bin/kula"
chmod 644 "${PKG_DIR}/etc/kula/config.example.yaml"
chmod 644 "${PKG_DIR}/usr/share/bash-completion/completions/kula"
chmod 644 "${PKG_DIR}/usr/share/man/man1/kula.1.gz"
chmod 644 "${PKG_DIR}/lib/systemd/system/kula.service"
find "${PKG_DIR}/usr/share/kula" -type f -exec chmod 644 {} +

echo "Building Debian package..."
dpkg-deb --root-owner-group --build "${PKG_DIR}"

# Move the package to current dir
mkdir -p dist
mv "${BUILD_DIR}/${PKG_NAME}_${VERSION}_${ARCH}.deb" dist/
rm -rf "$BUILD_DIR"

echo "Package built: ${PKG_NAME}_${VERSION}_${ARCH}.deb"
