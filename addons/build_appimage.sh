#!/usr/bin/env bash

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "${SCRIPT_DIR}/.."

ARCH=$1
if [ -z "$ARCH" ]; then
    ARCH=$(uname -m)
fi

case "$ARCH" in
    x86_64|amd64) ARCH="x86_64"; GOARCH="amd64" ;;
    aarch64|arm64) ARCH="aarch64"; GOARCH="arm64" ;;
    riscv64) ARCH="riscv64"; GOARCH="riscv64" ;;
    *) echo "Unsupported architecture: $ARCH" ; exit 1 ;;
esac

# VERSION
VERSION_FILE="VERSION"
if [ -f "${VERSION_FILE}" ]; then
    VERSION="$(head -1 "${VERSION_FILE}" | tr -d '[:space:]')"
else
    echo "Error: VERSION file not found"
    exit 1
fi

APP_NAME="kula"
BUILD_DIR="build_appimage"
APP_DIR="${BUILD_DIR}/AppDir"
OUTPUT_DIR="dist"

echo "Building AppImage for ${APP_NAME} v${VERSION} (${ARCH})..."

# Cleanup
rm -rf "${BUILD_DIR}"
mkdir -p "${APP_DIR}/usr/bin"
mkdir -p "${APP_DIR}/usr/share/icons/hicolor/scalable/apps"
mkdir -p "${APP_DIR}/usr/share/applications"

# Build binary
echo "Compiling for linux/${GOARCH}..."
CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH go build \
    -trimpath \
    -ldflags="-s -w" \
    -buildvcs=false \
    -o "${APP_DIR}/usr/bin/kula" \
    ./cmd/kula/

# AppRun
cat > "${APP_DIR}/AppRun" <<'EOF'
#!/bin/sh
SELF=$(readlink -f "$0")
HERE=${SELF%/*}
export PATH="${HERE}/usr/bin/:${PATH}"
exec "${HERE}/usr/bin/kula" "$@"
EOF
chmod +x "${APP_DIR}/AppRun"

# Desktop file
cat > "${APP_DIR}/kula.desktop" <<EOF
[Desktop Entry]
Name=KULA
Exec=kula
Icon=kula
Type=Application
Categories=System;Monitor;
Comment=Lightweight system monitoring tool
EOF

# Icon
ICON_SRC="internal/web/static/kula.svg"
if [ -f "$ICON_SRC" ]; then
    cp "$ICON_SRC" "${APP_DIR}/kula.svg"
    cp "$ICON_SRC" "${APP_DIR}/usr/share/icons/hicolor/scalable/apps/kula.svg"
    cp "$ICON_SRC" "${APP_DIR}/.DirIcon"
fi

# Include documentation and config in usr/share/kula
mkdir -p "${APP_DIR}/usr/share/kula"
for f in CHANGELOG.md VERSION README.md SECURITY.md LICENSE config.example.yaml; do
    if [ -f "$f" ]; then
        cp "$f" "${APP_DIR}/usr/share/kula/"
    fi
done

if [ -d "scripts" ]; then
    cp -r scripts "${APP_DIR}/usr/share/kula/"
fi

# Create AppImage
# Determine appimagetool command
APPIMAGETOOL_CMD="appimagetool"
if ! command -v "$APPIMAGETOOL_CMD" >/dev/null 2>&1; then
    # Check for local AppImage tool
    if [ -f "/opt/appimagetool-x86_64.appimage" ]; then
        chmod +x "/opt/appimagetool-x86_64.appimage"
        APPIMAGETOOL_CMD="/opt/appimagetool-x86_64.appimage"
    elif [ -f "/opt/appimagetool" ]; then
        chmod +x "/opt/appimagetool"
        APPIMAGETOOL_CMD="/opt/appimagetool"
    fi
fi

if command -v "$APPIMAGETOOL_CMD" >/dev/null 2>&1 || [ -f "$APPIMAGETOOL_CMD" ]; then
    echo "Creating AppImage using $APPIMAGETOOL_CMD..."
    mkdir -p "$OUTPUT_DIR"
    
    # Use absolute paths for the tool
    ABS_APP_DIR="$(readlink -f "${APP_DIR}")"
    ABS_OUTPUT_PATH="$(readlink -f "${OUTPUT_DIR}")/kula-${VERSION}-${ARCH}.AppImage"
    
    # AppImageRuntime expects ARCH environment variable
    ARCH=$ARCH VERSION=$VERSION "$APPIMAGETOOL_CMD" "$ABS_APP_DIR" "$ABS_OUTPUT_PATH"
else
    echo "appimagetool not found. To build the final AppImage file, please install it:"
    echo "  https://github.com/AppImage/AppImageKit/releases"
    echo ""
    echo "The AppDir structure is prepared at: ${APP_DIR}"
fi

echo "Done!"
