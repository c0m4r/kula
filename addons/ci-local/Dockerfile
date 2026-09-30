# Local stand-in for GitHub's ubuntu-latest runner, built by addons/ci-local.sh.
#
# Same Ubuntu release, a non-root `runner` user (uid 1001) with passwordless
# sudo, build-essential for cgo and the race detector, Google Chrome at
# /usr/bin/google-chrome, and a toolcache laid out like the hosted one
# (/opt/hostedtoolcache/<tool>/<version>/<arch>) holding the Go, Node.js and
# Python versions the workflows ask for, fetched from the same places the
# setup-go, setup-node and setup-python actions download from. ci-local.sh
# passes the versions in; a Node.js or Python spec such as 22 or 3.x resolves
# to the newest matching release when its layer is built, so --rebuild
# refreshes them the way GitHub refreshes its images.

ARG UBUNTU_VERSION=24.04
FROM ubuntu:${UBUNTU_VERSION}

SHELL ["/bin/bash", "-euo", "pipefail", "-c"]

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    RUNNER_TOOL_CACHE=/opt/hostedtoolcache

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        build-essential ca-certificates curl file git gnupg jq lsb-release \
        procps python3 python3-yaml sudo tzdata unzip wget xz-utils zip \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --uid 1001 --shell /bin/bash runner \
 && echo 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/runner \
 && chmod 0440 /etc/sudoers.d/runner \
 && mkdir -p "$RUNNER_TOOL_CACHE" /home/runner/work/_temp /home/runner/go/pkg/mod \
        /home/runner/.cache/go-build /home/runner/.cache/pip /home/runner/.npm \
 && chown -R runner:runner "$RUNNER_TOOL_CACHE" /home/runner

# The hosted image installs Chrome from Google's package, which only exists
# for amd64; on other platforms the browser jobs fail to find a browser.
# Google's signing key arrives over the same connection as the packages it
# vouches for, so it is pinned by its primary fingerprint (the subkeys rotate)
# and must be the only primary key in the file.
ARG GOOGLE_LINUX_KEY_FPR=EB4C1BFD4F042F6DDDCCEC917721F63BD38B4796
RUN if [ "$(dpkg --print-architecture)" = amd64 ]; then \
        key=$(mktemp); \
        curl -fsSL -o "$key" https://dl.google.com/linux/linux_signing_key.pub; \
        fprs=$(GNUPGHOME=$(mktemp -d) gpg --show-keys --with-colons "$key" \
            | awk -F: '$1 == "pub" { primary = 1; next } $1 == "fpr" && primary { print $10; primary = 0 }'); \
        if [ "$fprs" != "$GOOGLE_LINUX_KEY_FPR" ]; then \
            echo "Google's signing key has primary fingerprint(s) [$fprs], expected $GOOGLE_LINUX_KEY_FPR;" \
                "check https://www.google.com/linux/linuxrepositories/ and update GOOGLE_LINUX_KEY_FPR" >&2; \
            exit 1; \
        fi; \
        gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg "$key"; \
        rm -f "$key"; \
        echo 'deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] https://dl.google.com/linux/chrome/deb/ stable main' \
            > /etc/apt/sources.list.d/google-chrome.list; \
        apt-get update; \
        apt-get install -y --no-install-recommends google-chrome-stable; \
        rm -rf /var/lib/apt/lists/*; \
    else \
        echo "Google Chrome has no $(dpkg --print-architecture) build; skipping it" >&2; \
    fi

# The actions/python-versions builds setup-python installs, placed by their
# own setup.sh. The manifest publishes no checksums.
ARG PYTHON_VERSIONS=""
RUN . /etc/os-release; \
    arch=$(dpkg --print-architecture | sed 's/^amd64$/x64/'); \
    manifest=$(curl -fsSL https://raw.githubusercontent.com/actions/python-versions/main/versions-manifest.json); \
    for spec in $PYTHON_VERSIONS; do \
        case $spec in *.x) prefix=${spec%x} ;; *) prefix=$spec. ;; esac; \
        url=$(jq -r --arg s "$spec" --arg p "$prefix" --arg a "$arch" --arg pv "$VERSION_ID" \
            'first(.[] | select(.stable and (.version == $s or (.version | startswith($p)))) | .files[] \
                   | select(.platform == "linux" and .arch == $a and .platform_version == $pv)) | .download_url' \
            <<<"$manifest"); \
        [ -n "$url" ] || { echo "No actions/python-versions build of Python $spec for Ubuntu $VERSION_ID $arch" >&2; exit 1; }; \
        work=$(mktemp -d); \
        curl -fsSL "$url" | tar -xz -C "$work"; \
        (cd "$work" && bash ./setup.sh >/dev/null); \
        rm -rf "$work"; \
    done; \
    chown -R runner:runner "$RUNNER_TOOL_CACHE"

# nodejs.org release tarballs, the source setup-node falls back to, checked
# against the release's SHASUMS256.txt.
ARG NODE_VERSIONS=""
RUN arch=$(dpkg --print-architecture | sed 's/^amd64$/x64/'); \
    index=$(curl -fsSL https://nodejs.org/dist/index.json); \
    for spec in $NODE_VERSIONS; do \
        spec=${spec#v}; \
        case $spec in *.x) prefix=v${spec%x} ;; *) prefix=v$spec. ;; esac; \
        version=$(jq -r --arg s "v$spec" --arg p "$prefix" \
            'first(.[] | select(.version == $s or (.version | startswith($p)))) | .version' <<<"$index"); \
        [ -n "$version" ] || { echo "No Node.js release matches $spec" >&2; exit 1; }; \
        file=node-$version-linux-$arch.tar.xz; \
        work=$(mktemp -d); \
        curl -fsSL -o "$work/$file" "https://nodejs.org/dist/$version/$file"; \
        curl -fsSL "https://nodejs.org/dist/$version/SHASUMS256.txt" | grep " $file\$" | (cd "$work" && sha256sum -c --quiet -); \
        dest=$RUNNER_TOOL_CACHE/node/${version#v}/$arch; \
        mkdir -p "$dest"; \
        tar -xJ --strip-components=1 -C "$dest" -f "$work/$file"; \
        touch "$dest.complete"; \
        rm -rf "$work"; \
    done; \
    chown -R runner:runner "$RUNNER_TOOL_CACHE"

# go.dev release archives, checked against the SHA-256 go.dev publishes.
ARG GO_VERSIONS=""
RUN arch=$(dpkg --print-architecture); \
    toolarch=$(sed 's/^amd64$/x64/' <<<"$arch"); \
    releases=$(curl -fsSL 'https://go.dev/dl/?mode=json&include=all'); \
    for spec in $GO_VERSIONS; do \
        spec=${spec#go}; \
        case $spec in *.x) prefix=go${spec%x} ;; *) prefix=go$spec. ;; esac; \
        release=$(jq -r --arg s "go$spec" --arg p "$prefix" --arg a "$arch" \
            'first(.[] | select(.stable and (.version == $s or (.version | startswith($p))))) | .files[] \
             | select(.os == "linux" and .arch == $a and .kind == "archive") | "\(.version) \(.filename) \(.sha256)"' \
            <<<"$releases"); \
        [ -n "$release" ] || { echo "No Go release matches $spec" >&2; exit 1; }; \
        read -r version file sha <<<"$release"; \
        work=$(mktemp -d); \
        curl -fsSL -o "$work/$file" "https://go.dev/dl/$file"; \
        echo "$sha  $work/$file" | sha256sum -c --quiet -; \
        dest=$RUNNER_TOOL_CACHE/go/${version#go}/$toolarch; \
        mkdir -p "$dest"; \
        tar -xz --strip-components=1 -C "$dest" -f "$work/$file"; \
        touch "$dest.complete"; \
        rm -rf "$work"; \
    done; \
    chown -R runner:runner "$RUNNER_TOOL_CACHE"

# The checkout's file modes are not trustworthy (a fresh script is often 0600),
# so make the runner readable and the entrypoint executable explicitly.
COPY entrypoint.sh run-workflow.py /opt/ci-local/
RUN chmod 0755 /opt/ci-local/entrypoint.sh \
 && chmod 0644 /opt/ci-local/run-workflow.py

USER runner
WORKDIR /home/runner
ENTRYPOINT ["/bin/bash", "/opt/ci-local/entrypoint.sh"]
