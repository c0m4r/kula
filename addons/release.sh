#!/usr/bin/env bash

set -e

cd "$(dirname "$0")"

VERSION=$(cat ../VERSION)

if [ -z "$VERSION" ]; then
    echo "Error: VERSION file not found"
    exit 1
fi

./build.sh cross

cd "../dist"

for f in kula-linux-* ; do
    mkdir -p kula
    cp "$f" kula/kula
    cp ../VERSION kula/
    cp ../LICENSE kula/
    cp ../README.md kula/
    cp ../config.example.yaml kula/
    cp -r ../addons/bash-completion kula/
    cp -r ../addons/init kula/
    cp -r ../addons/man kula/
    g="$(echo "$f" | sed 's/-linux//g;')"
    tar -czf "${g}.tar.gz" kula
    rm -rf kula
done

cd ..

# building deb packages for all archs
for arch in amd64 arm64 riscv64; do
    ./addons/build_deb.sh "$arch"
done

# generate AUR files
echo -e "2\n" | ./addons/build_aur.sh

cd -

tar -czf kula-$VERSION-aur.tar.gz kula-$VERSION-aur

echo "Release done!"
pwd
ls -1 $(pwd)/*.tar.gz
ls -1 $(pwd)/*.deb
