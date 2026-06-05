#!/usr/bin/env bash

set -e

# Check if docker command exists
if ! command -v docker &>/dev/null; then
    echo "Error: docker is not installed."
    echo ""
    echo "Install Docker:"
    echo "  Debian/Ubuntu: sudo apt install docker.io"
    echo "  Arch Linux:    sudo pacman -S docker"
    echo "  Fedora:        sudo dnf install docker"
    echo "  Or visit:      https://docs.docker.com/engine/install/"
    exit 1
fi

# Build the docker image
# Assuming this script is run from the project root or the docker/ dir
# If run from docker/, we need to tell docker to use the parent dir for context
cd "$(dirname "$0")/../.."

# Read version from VERSION file
VERSION=$(cat VERSION | tr -d '[:space:]')

# Allow building for a specific platform if provided as an argument
PLATFORM=${1:-""}
TAG="kula:$VERSION"

if [ -n "$PLATFORM" ]; then
    echo "Building Docker image '$TAG' for platform '$PLATFORM' for local review..."
    docker buildx build --platform "$PLATFORM" -t "$TAG" --load -f addons/docker/Dockerfile .
else
    echo "Building Docker image '$TAG' for host architecture..."
    docker buildx build -t "$TAG" --load -f addons/docker/Dockerfile .
fi

echo "Done!"
