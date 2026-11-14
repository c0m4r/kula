# Snapcraft in Docker, built and run by `addons/build_snap.sh --docker`.
#
# Canonical's snapcraft rock: Snapcraft 8 for core24 snaps on Ubuntu 24.04,
# whose pebble entrypoint runs `snapcraft <args>` in /project in destructive
# mode (SNAPCRAFT_BUILD_ENVIRONMENT=host), so no LXD is involved. Snapcraft
# skips build-snaps inside a Docker container, so the `go` build snap that
# snap/snapcraft.yaml asks for has to be in the image: the Go release go.mod
# names, which build_snap.sh passes in as GO_VERSION.
FROM ghcr.io/canonical/snapcraft:8_core24@sha256:f51fc5081f844bece0bb69d7b19a9c543f4666685da630dd9f9f7d0fb74e08f5

SHELL ["/bin/bash", "-euo", "pipefail", "-c"]

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl jq \
 && rm -rf /var/lib/apt/lists/*

# go.dev release archive, checked against the SHA-256 go.dev publishes. The
# entrypoint sets its own PATH, so the tools are linked into /usr/local/bin.
ARG GO_VERSION
RUN [ -n "${GO_VERSION:-}" ] || { echo "GO_VERSION is not set" >&2; exit 1; }; \
    arch=$(dpkg --print-architecture); \
    spec=${GO_VERSION#go}; \
    release=$(curl -fsSL 'https://go.dev/dl/?mode=json&include=all' \
        | jq -r --arg s "go$spec" --arg p "go$spec." --arg a "$arch" \
            'first(.[] | select(.stable and (.version == $s or (.version | startswith($p))))) | .files[] \
             | select(.os == "linux" and .arch == $a and .kind == "archive") | "\(.filename) \(.sha256)"'); \
    [ -n "$release" ] || { echo "No Go release matches $spec for linux/$arch" >&2; exit 1; }; \
    read -r file sha <<<"$release"; \
    work=$(mktemp -d); \
    curl -fsSL -o "$work/$file" "https://go.dev/dl/$file"; \
    echo "$sha  $work/$file" | sha256sum -c --quiet -; \
    tar -xz -C /usr/local -f "$work/$file"; \
    rm -rf "$work"; \
    ln -s /usr/local/go/bin/go /usr/local/go/bin/gofmt /usr/local/bin/; \
    go version
