#!/usr/bin/env bash
# check_helpers.sh - run each packaging helper on a scratch copy of the tree
# and check that the result still vets and passes the web tests, as a distro
# build that runs them and then `go test` would. The working tree itself is
# never modified.
#
#   ./addons/packaging/check_helpers.sh
#
# replace_chartjs.sh is not covered: it needs the upstream Chart.js files
# (--from DIR or --download); run it by hand, then ./addons/check.sh.

set -euo pipefail

GREEN="\033[0;32m"
CYAN="\033[0;36m"
RESET="\033[0m"

cd "$(dirname "$0")/../.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Tracked and untracked files, minus ignored ones: the tree as it is now.
snapshot() { # DEST
    mkdir -p "$1"
    git ls-files -z --cached --others --exclude-standard \
        | tar --null --files-from=- --ignore-failed-read --create --file=- 2>/dev/null \
        | tar --extract --file=- --directory="$1"
}

check_case() { # NAME HELPER...
    local name=$1 dest="$work/$1"
    shift
    echo -e "${CYAN}${name}: $*${RESET}"
    snapshot "$dest"
    for helper in "$@"; do
        (cd "$dest" && "./addons/packaging/$helper" >/dev/null)
    done
    (cd "$dest" && go vet ./... && go test ./internal/web/)
}

check_case fonts remove_fonts.sh
check_case game remove_game.sh
check_case game-then-fonts remove_game.sh remove_fonts.sh
check_case fonts-then-game remove_fonts.sh remove_game.sh

echo -e "${GREEN}Every packaging helper leaves a tree that vets and passes the web tests${RESET}"
