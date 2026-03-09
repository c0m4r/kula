#!/usr/bin/env bash

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd ${SCRIPT_DIR}/..

ARCH=$1

if [ -z "$ARCH" ]; then
    ARCH=$(cat /proc/sys/kernel/arch)
    case "$ARCH" in
        x86_64) ARCH="x86_64" ;;
        aarch64) ARCH="aarch64" ;;
        riscv64) ARCH="riscv64" ;;
        *) echo "Unsupported architecture: $ARCH" ; exit 1 ;;
    esac
else
    # Map go arch to rpm arch
    case "$ARCH" in
        amd64) ARCH="x86_64" ;;
        arm64) ARCH="aarch64" ;;
        riscv64) ARCH="riscv64" ;;
        x86_64|aarch64) ;; # ok
        *) echo "Unsupported architecture: $ARCH" ; exit 1 ;;
    esac
fi

# Map back to go arch for finding the binary
GOARCH=$ARCH
case "$ARCH" in
    x86_64) GOARCH="amd64" ;;
    aarch64) GOARCH="arm64" ;;
esac

# Check if rpmbuild is installed
if ! command -v rpmbuild &>/dev/null; then
    echo "Error: rpmbuild is not installed."
    echo "Install rpm-build:"
    echo "  Fedora/RHEL:   sudo dnf install rpm-build"
    echo "  Arch Linux:    sudo pacman -S rpm-tools"
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
BUILD_DIR="build_rpm"
RPM_ROOT="${PWD}/${BUILD_DIR}/rpmbuild/${PKG_NAME}-${VERSION}-${ARCH}"

# Check if binary exists, build if not
if [ ! -f "dist/kula-linux-${VERSION}-${GOARCH}" ]; then
    echo "kula binary not found, building first..."
    ${SCRIPT_DIR}/build.sh cross
fi

echo "Building RPM package v${VERSION} for ${ARCH}..."

echo "Cleaning up old build directory..."
rm -rf "${BUILD_DIR}"

echo "Creating directory structure for rpmbuild..."
mkdir -p "${RPM_ROOT}/BUILD"
mkdir -p "${RPM_ROOT}/RPMS"
mkdir -p "${RPM_ROOT}/SOURCES"
mkdir -p "${RPM_ROOT}/SPECS"
mkdir -p "${RPM_ROOT}/SRPMS"
mkdir -p "${RPM_ROOT}/BUILDROOT"

PKG_DIR="${RPM_ROOT}/BUILDROOT"

mkdir -p "${PKG_DIR}/usr/bin"
mkdir -p "${PKG_DIR}/etc/kula"
mkdir -p "${PKG_DIR}/var/lib/kula"
mkdir -p "${PKG_DIR}/usr/share/kula"
mkdir -p "${PKG_DIR}/usr/share/bash-completion/completions"
mkdir -p "${PKG_DIR}/usr/share/man/man1"
mkdir -p "${PKG_DIR}/usr/lib/systemd/system"

echo "Copying files..."
cp dist/kula-linux-${VERSION}-${GOARCH} "${PKG_DIR}/usr/bin/kula"
cp config.example.yaml "${PKG_DIR}/etc/kula/config.example.yaml"
cp addons/bash-completion/kula "${PKG_DIR}/usr/share/bash-completion/completions/kula"
cp addons/init/systemd/kula.service "${PKG_DIR}/usr/lib/systemd/system/kula.service"

for f in CHANGELOG VERSION README.md SECURITY.md LICENSE config.example.yaml; do
    if [ -f "$f" ]; then
        cp "$f" "${PKG_DIR}/usr/share/kula/"
    fi
done

# Compress and copy man page
gzip -c addons/man/kula.1 > "${PKG_DIR}/usr/share/man/man1/kula.1.gz"

# Create a local RPM DB directory so it doesn't try to lock /var/lib/rpm on Arch Linux
mkdir -p "${RPM_ROOT}/rpmdb"
rpmdb --initdb --dbpath "${RPM_ROOT}/rpmdb"

echo "Creating RPM spec file..."
cat <<EOF > "${RPM_ROOT}/SPECS/${PKG_NAME}.spec"
Name:           ${PKG_NAME}
Version:        ${VERSION}
Release:        1%{?dist}
Summary:        ${DESCRIPTION}
License:        AGPL-3.0-or-later
URL:            https://github.com/c0m4r/kula

Requires(pre):  shadow-utils
Provides:       user(${PKG_NAME})
Provides:       group(${PKG_NAME})

%description
${DESCRIPTION}

%install
# rpmbuild cleans the buildroot, so we copy files into it here during the install phase
mkdir -p %{buildroot}/usr/bin
mkdir -p %{buildroot}/etc/kula
mkdir -p %{buildroot}/var/lib/kula
mkdir -p %{buildroot}/usr/share/kula
mkdir -p %{buildroot}/usr/share/bash-completion/completions
mkdir -p %{buildroot}/usr/share/man/man1
mkdir -p %{buildroot}/usr/lib/systemd/system

cp ${SCRIPT_DIR}/../dist/kula-linux-${VERSION}-${GOARCH} %{buildroot}/usr/bin/kula
cp ${SCRIPT_DIR}/../config.example.yaml %{buildroot}/etc/kula/config.example.yaml
cp ${SCRIPT_DIR}/../addons/bash-completion/kula %{buildroot}/usr/share/bash-completion/completions/kula
cp ${SCRIPT_DIR}/../addons/init/systemd/kula.service %{buildroot}/usr/lib/systemd/system/kula.service

for f in CHANGELOG VERSION README.md SECURITY.md LICENSE config.example.yaml; do
    if [ -f "${SCRIPT_DIR}/../\$f" ]; then
        cp "${SCRIPT_DIR}/../\$f" "%{buildroot}/usr/share/kula/"
    fi
done

gzip -c ${SCRIPT_DIR}/../addons/man/kula.1 > %{buildroot}/usr/share/man/man1/kula.1.gz

# Set permissions for the files inside the buildroot
chmod 755 %{buildroot}/usr/bin/kula
chmod 644 %{buildroot}/etc/kula/config.example.yaml
chmod 644 %{buildroot}/usr/share/bash-completion/completions/kula
chmod 644 %{buildroot}/usr/share/man/man1/kula.1.gz
chmod 644 %{buildroot}/usr/lib/systemd/system/kula.service

%pre
# Pre-install script
# Create kula group if it doesn't exist
if ! getent group kula >/dev/null; then
    groupadd --system kula
fi

# Create kula user if it doesn't exist
if ! getent passwd kula >/dev/null; then
    useradd --system -g kula -d /var/lib/kula -s /sbin/nologin -c "Kula System Monitoring Daemon" kula
fi

%post
# Post-install script
# Set ownership for directories the program will use
chown -R kula:kula /etc/kula
chown -R kula:kula /var/lib/kula

# Load systemd, enable and start service
if command -v systemctl >/dev/null; then
    systemctl daemon-reload || true
    systemctl enable kula.service || true
    systemctl start kula.service || true
fi

%preun
# Pre-uninstall script
if [ "\$1" = "0" ]; then # Only if it's a removal, not upgrade
    if command -v systemctl >/dev/null; then
        systemctl stop kula.service || true
        systemctl disable kula.service || true
    fi
fi

%postun
# Post-uninstall script

%files
%defattr(-,root,root,-)
%attr(755, root, root) /usr/bin/kula
%dir %attr(750, kula, kula) /etc/kula
%config(noreplace) %attr(644, root, root) /etc/kula/config.example.yaml
%attr(644, root, root) /usr/share/bash-completion/completions/kula
%attr(644, root, root) /usr/lib/systemd/system/kula.service
%attr(644, root, root) /usr/share/man/man1/kula.1.gz
%dir %attr(750, kula, kula) /var/lib/kula
%dir /usr/share/kula
/usr/share/kula/*

%changelog
* Sun Mar 08 2026 Admin <admin@example.com> - ${VERSION}-1
- Initial RPM release
EOF

# Set proper permissions in BUILDROOT
chmod 755 "${PKG_DIR}/usr/bin/kula"
chmod 644 "${PKG_DIR}/etc/kula/config.example.yaml"
chmod 644 "${PKG_DIR}/usr/share/bash-completion/completions/kula"
chmod 644 "${PKG_DIR}/usr/share/man/man1/kula.1.gz"
chmod 644 "${PKG_DIR}/usr/lib/systemd/system/kula.service"

# Limit build environment info
# Set fixed build time if not already set for reproducibility
export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(date +%s)}"

echo "Building RPM package..."
rpmbuild --define "_topdir ${RPM_ROOT}" \
         --define "_buildrootdir ${PKG_DIR}" \
         --define "_buildhost localhost" \
         --define "packager ${MAINTAINER}" \
         --dbpath "${RPM_ROOT}/rpmdb" \
         --target "${ARCH}" \
         -bb "${RPM_ROOT}/SPECS/${PKG_NAME}.spec"

# Move and rename the package to current dir (kula-version-arch.rpm)
mkdir -p dist
find "${RPM_ROOT}/RPMS" -name "*.rpm" -exec mv {} "dist/kula-${VERSION}-${ARCH}.rpm" \;
rm -rf "$BUILD_DIR"

echo "Package built successfully: dist/kula-${VERSION}-${ARCH}.rpm"
ls -l "dist/kula-${VERSION}-${ARCH}.rpm"
