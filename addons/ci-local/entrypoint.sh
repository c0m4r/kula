#!/usr/bin/env bash
# Entrypoint of the addons/ci-local.sh runner image. Unpacks the source
# snapshot mounted at /ci/src.tar into the workspace path GitHub uses and
# commits it, so the workspace is a git repository like actions/checkout
# leaves, then lists jobs, runs one, or opens a shell.
#
#   list WORKFLOW...    run-workflow.py list
#   run WORKFLOW JOB    run-workflow.py run
#   shell               interactive bash with the newest toolcache Go,
#                       Node.js and Python on PATH

set -euo pipefail

repo=${CI_LOCAL_REPOSITORY:-kula}
repo=${repo##*/}
repo=${repo%.git}
case $repo in
    "" | . | .. | -*) repo=kula ;;
esac
export GITHUB_WORKSPACE=/home/runner/work/${repo:-kula}/${repo:-kula}
export RUNNER_TEMP=/home/runner/work/_temp

# A named volume takes its owner from the image directory it is first mounted
# on, and one created before the image had that directory is root-owned.
for cache in "$HOME/go" "$HOME/.cache" "$HOME/.npm"; do
    if [ -d "$cache" ] && [ ! -w "$cache" ]; then
        sudo -n chown runner:runner "$cache"
    fi
done

mkdir -p "$GITHUB_WORKSPACE" "$RUNNER_TEMP"
tar -xf /ci/src.tar -C "$GITHUB_WORKSPACE"
cd "$GITHUB_WORKSPACE"
git init -q -b "${CI_LOCAL_REF_NAME:-main}"
git add -A
git -c user.name=ci-local -c user.email=ci-local@localhost -c commit.gpgsign=false \
    commit -q --no-verify --allow-empty -m "ci-local snapshot of ${CI_LOCAL_SHA:-the working tree}"

case "${1:-}" in
    shell)
        for tool in Python node go; do
            dir=$(find "$RUNNER_TOOL_CACHE/$tool" -mindepth 2 -maxdepth 2 -type d 2>/dev/null | sort -V | tail -n 1)
            [ -n "$dir" ] && PATH=$dir/bin:$PATH
        done
        export PATH=$HOME/go/bin:$PATH CI=true CI_LOCAL=true
        echo "Workspace: $GITHUB_WORKSPACE (a fresh snapshot, discarded on exit)"
        echo "Tools: $(go version 2>/dev/null | cut -d' ' -f3), node $(node --version 2>/dev/null), $(python --version 2>/dev/null)"
        exec bash
        ;;
    *)
        exec /usr/bin/python3 /opt/ci-local/run-workflow.py "$@"
        ;;
esac
