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
    echo "Error: VERSION file not found"
    exit 1
fi

# Configuration
PKG_NAME="kula"
MAINTAINER="c0m4r"
DESCRIPTION="Lightweight system monitoring daemon."
BUILD_DIR="build_deb"
PKG_DIR="${BUILD_DIR}/${PKG_NAME}-${VERSION}-${ARCH}"

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

if [ -d "scripts" ]; then
    cp -r scripts "${PKG_DIR}/usr/share/kula/"
fi

for f in CHANGELOG.md VERSION README.md SECURITY.md LICENSE config.example.yaml; do
    if [ -f "$f" ]; then
        cp "$f" "${PKG_DIR}/usr/share/kula/"
    fi
done

# Compress and copy man page
gzip -c addons/man/kula.1 > "${PKG_DIR}/usr/share/man/man1/kula.1.gz"

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

    # The config holds the password hash, tokens and database passwords: root
    # owns it and the daemon only reads it, through its group. Seed it from
    # the packaged example on first install. Older packages gave /etc/kula to
    # the daemon, so once the directory is root's (nothing can swap its
    # entries after that), tighten an existing config only if it is a plain
    # file with one link, never a link the daemon could have left there.
    chown root:kula /etc/kula
    chmod 0750 /etc/kula
    if [ ! -e /etc/kula/config.yaml ] && [ ! -L /etc/kula/config.yaml ]; then
        install -m 0640 -o root -g kula /etc/kula/config.example.yaml /etc/kula/config.yaml
    elif [ -f /etc/kula/config.yaml ] && [ ! -L /etc/kula/config.yaml ] && [ "\$(stat -c %h /etc/kula/config.yaml)" = 1 ]; then
        chown root:kula /etc/kula/config.yaml
        chmod 0640 /etc/kula/config.yaml
    else
        echo "kula: /etc/kula/config.yaml is a link or not a plain file; its owner and mode are left as they are" >&2
    fi

    # Storage belongs to the daemon
    chown -R kula:kula /var/lib/kula
    chmod 0750 /var/lib/kula

    # Load the unit. A first install (no previously configured version in
    # \$2) enables and starts the service. An upgrade, on a host systemd
    # runs, restarts the service only if it is running, so the new version
    # takes over, and leaves whether it is enabled to the administrator.
    if command -v systemctl >/dev/null; then
        systemctl daemon-reload || true
        if [ -z "\$2" ]; then
            systemctl enable kula.service || true
            systemctl start kula.service || true
        elif [ -d /run/systemd/system ]; then
            if ! systemctl try-restart kula.service; then
                echo "kula: kula.service failed to restart after the upgrade; see journalctl -u kula" >&2
            fi
        fi
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
chmod 750 "${PKG_DIR}/etc/kula"
chmod 750 "${PKG_DIR}/var/lib/kula"
chmod 755 "${PKG_DIR}/usr/bin/kula"
chmod 644 "${PKG_DIR}/etc/kula/config.example.yaml"
chmod 644 "${PKG_DIR}/usr/share/bash-completion/completions/kula"
chmod 644 "${PKG_DIR}/usr/share/man/man1/kula.1.gz"
chmod 644 "${PKG_DIR}/lib/systemd/system/kula.service"
find "${PKG_DIR}/usr/share/kula" -type f -exec chmod 644 {} +
# The helper scripts are run directly, as the rpm and AUR packages ship them
if [ -d "${PKG_DIR}/usr/share/kula/scripts" ]; then
    find "${PKG_DIR}/usr/share/kula/scripts" -type f -exec chmod 755 {} +
fi

echo "Building Debian package..."
dpkg-deb --root-owner-group --build "${PKG_DIR}"

# Move the package to current dir
mkdir -p dist
mv "${BUILD_DIR}/${PKG_NAME}-${VERSION}-${ARCH}.deb" dist/
rm -rf "$BUILD_DIR"

echo "Package built: ${PKG_NAME}-${VERSION}-${ARCH}.deb"
