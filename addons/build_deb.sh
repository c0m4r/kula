#!/usr/bin/env bash

set -e

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
    echo "Warning: VERSION file not found, using default 0.1.0"
    VERSION="0.1.0"
fi

# Configuration
PKG_NAME="kula"
ARCH="amd64"
MAINTAINER="c0m4r"
DESCRIPTION="Lightweight system monitoring daemon."
BUILD_DIR="build_deb"
PKG_DIR="${BUILD_DIR}/${PKG_NAME}_${VERSION}_${ARCH}"

# Check if binary exists, build if not
if [ ! -f "kula" ]; then
    echo "kula binary not found, building first..."
    ./build.sh
fi

echo "Building Debian package v${VERSION}..."

echo "Cleaning up old build directory..."
rm -rf "${BUILD_DIR}"

echo "Creating directory structure..."
mkdir -p "${PKG_DIR}/DEBIAN"
mkdir -p "${PKG_DIR}/usr/bin"
mkdir -p "${PKG_DIR}/etc/kula"
mkdir -p "${PKG_DIR}/var/lib/kula"
mkdir -p "${PKG_DIR}/usr/share/bash-completion/completions"
mkdir -p "${PKG_DIR}/usr/share/man/man1"

echo "Copying files..."
cp kula "${PKG_DIR}/usr/bin/kula"
cp config.example.yaml "${PKG_DIR}/etc/kula/config.example.yaml"
cp docs/kula-completion.bash "${PKG_DIR}/usr/share/bash-completion/completions/kula"

# Compress and copy man page
gzip -c docs/kula.1 > "${PKG_DIR}/usr/share/man/man1/kula.1.gz"

echo "Creating DEBIAN control file..."
cat <<EOF > "${PKG_DIR}/DEBIAN/control"
Package: ${PKG_NAME}
Version: ${VERSION}
Architecture: ${ARCH}
Maintainer: ${MAINTAINER}
Description: ${DESCRIPTION}
EOF

# Set proper permissions
chmod 755 "${PKG_DIR}/usr/bin/kula"
chmod 644 "${PKG_DIR}/etc/kula/config.example.yaml"
chmod 644 "${PKG_DIR}/usr/share/bash-completion/completions/kula"
chmod 644 "${PKG_DIR}/usr/share/man/man1/kula.1.gz"

echo "Building Debian package..."
dpkg-deb --build "${PKG_DIR}"

# Move the package to current dir
mv "${BUILD_DIR}/${PKG_NAME}_${VERSION}_${ARCH}.deb" .

echo "Package built: ${PKG_NAME}_${VERSION}_${ARCH}.deb"
