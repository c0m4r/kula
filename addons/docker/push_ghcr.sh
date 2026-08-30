#!/usr/bin/env bash

set -e

# Check if docker command exists
if ! command -v docker &>/dev/null; then
    echo "Error: docker is not installed."
    exit 1
fi

if ! docker buildx version &>/dev/null; then
    echo "Error: Docker Buildx is not available."
    echo "Install or enable the Docker Buildx plugin: https://docs.docker.com/reference/cli/docker/buildx/"
    exit 1
fi

cd "$(dirname "$0")/../.."

# Read version from VERSION file
VERSION=$(cat VERSION | tr -d '[:space:]')

PLATFORMS="linux/amd64,linux/arm64,linux/riscv64"

echo "Building and pushing multi-arch image for platforms: $PLATFORMS"
echo "Tags: c0m4r/kula:$VERSION, c0m4r/kula:latest"

# Ensure we have a builder that supports multi-platform
if ! docker buildx inspect kula-builder &>/dev/null; then
    docker buildx create --name kula-builder --use
fi

# Login
#docker login -u c0m4r

# Build and push using buildx
docker buildx build \
  --builder kula-builder \
  --platform "$PLATFORMS" \
  -t ghcr.io/c0m4r/kula:"$VERSION" \
  -t ghcr.io/c0m4r/kula:latest \
  -f addons/docker/Dockerfile \
  --push .

echo "Done!"
