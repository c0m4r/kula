#!/usr/bin/env bash

set -e

cd "$(dirname "$0")"

VERSION=$(cat ../VERSION)

if [ -z "$VERSION" ]; then
    echo "Error: VERSION file not found"
    exit 1
fi

ARCHS=(amd64 arm64 riscv64)

# Start from a clean dist so CHECKSUMS reflects exactly this release and no
# stale artifacts from a previous run leak in.
rm -rf ../dist

./build.sh cross

cd "../dist"

# Bundle tarballs + single gzipped binaries (uses the raw kula-linux-* binaries)
for f in kula-linux-* ; do
    mkdir -p kula
    cp "$f" kula/kula
    cp ../CHANGELOG.md kula/
    cp ../VERSION kula/
    cp ../LICENSE kula/
    cp ../README.md kula/
    cp ../config.example.yaml kula/
    cp -r ../scripts kula/
    cp -r ../addons/bash-completion kula/
    cp -r ../addons/init kula/
    cp -r ../addons/man kula/
    cd kula
    ln -s . addons
    cd -
    gzip -c "$f" > "${f}.gz"
    g="$(echo "$f" | sed 's/-linux//g;')"
    # Root ownership in the archive: extracted as root (the installer's /opt
    # path, or by hand), the builder's uid would otherwise own the binary.
    tar --owner=0 --group=0 --numeric-owner -czf "${g}.tar.gz" kula
    rm -rf kula
done

cd ..

# building deb and rpm packages for all archs (these also consume the raw binaries)
for arch in "${ARCHS[@]}"; do
    ./addons/build_deb.sh "$arch"
    ./addons/build_rpm.sh "$arch"
done

# AUR is intentionally not built here: its PKGBUILD pins the GitHub source
# archive that only exists once the release is published. Generate it afterwards
# with ./addons/build_aur.sh (it will append its checksums to CHECKSUMS.sha256.txt).

# Remove the uncompressed raw binaries; we ship the .gz/.tar.gz/.deb/.rpm only.
for arch in "${ARCHS[@]}"; do
    rm -f "dist/kula-linux-${VERSION}-${arch}"
done

# Snap, cross-built from source for all archs by the go plugin (so it ignores
# the binaries above): in Docker when it is usable, else in snapcraft's LXD
# instances. Use `./addons/build_snap.sh --remote` for native Launchpad builds
# instead. Skipped if neither docker nor snapcraft is installed so the release
# still completes.
if command -v docker >/dev/null 2>&1 || command -v snapcraft >/dev/null 2>&1; then
    ./addons/build_snap.sh cross || echo "Warning: snap build failed, skipping"
else
    echo "Notice: neither docker nor snapcraft found — skipping snap build"
fi

# Generate checksums over the distributable artifacts (very last step).
# Run from inside dist/ so filenames are bare; -type f skips the aur dir;
# the find avoids the "*.tar.gz also matches *.gz" double-listing trap.
( cd dist && find . -maxdepth 1 -type f -name 'kula-*' ! -name 'CHECKSUMS*' \
    -printf '%P\n' | sort | xargs sha256sum > CHECKSUMS.sha256.txt )

echo "Release done!"
echo "Artifacts in $(pwd)/dist (CHECKSUMS.sha256.txt):"
cat "$(pwd)/dist/CHECKSUMS.sha256.txt"
