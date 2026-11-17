#!/usr/bin/env bash
# Show what a change to addons/chartjs/ changes in the Chart.js bundle's code.
#
#   addons/chartjs/diff-bundle.sh                    HEAD vs the work tree
#   addons/chartjs/diff-bundle.sh OLD [NEW]          OLD vs NEW (default: the work tree)
#   addons/chartjs/diff-bundle.sh HEAD~1 HEAD --stat options go to git diff
#
# chartjs-bundle.min.js cannot be diffed usefully: esbuild names minified
# symbols by how often they are used, so any upstream change renames symbols
# throughout the bundle. This builds the bundle from addons/chartjs/ at both
# revisions the way addons/build-chartjs.sh does, but without --minify, and
# diffs the two. CI holds the committed bundle to the minified build of the
# same inputs, so this diff is what the minified one would show.
#
# Both sides install under the work tree's .npmrc guards and verify-lock.js,
# and build with the esbuild flags below (keep them in step with build-chartjs.sh).

set -euo pipefail

CYAN="\033[0;36m"
RESET="\033[0m"

usage() {
    sed -n '4,6s/^# *//p' "$0" >&2
    exit 2
}

cd "$(dirname "$0")/../.."
src=addons/chartjs

revs=() opts=()
for arg; do
    case $arg in
    -h | --help) usage ;;
    -*) opts+=("$arg") ;;
    *) revs+=("$arg") ;;
    esac
done
[ ${#revs[@]} -le 2 ] || usage

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

age=$(sed -n 's/^min-release-age=//p' "$src"/.npmrc)
npm_flags=(--ignore-scripts --min-release-age="$age" --no-audit --no-fund --loglevel=error)

# label REV prints the short commit REV names, or "worktree" for an empty REV.
label() {
    if [ -z "$1" ]; then
        echo worktree
    else
        git rev-parse --verify --quiet --short "$1^{commit}" ||
            { echo "$1: not a commit" >&2; exit 2; }
    fi
}

# build REV LABEL writes the unminified bundle of addons/chartjs/ at REV (the
# work tree when REV is empty) to $work/chartjs-bundle.LABEL.js.
build() {
    local dir="$work/$2"
    mkdir "$dir"
    echo -e "${CYAN}Building the unminified bundle at $2...${RESET}" >&2
    if [ -z "$1" ]; then
        tar -C "$src" --exclude=node_modules -cf - . | tar -C "$dir" -xf -
    else
        git cat-file -e "$1:$src/package-lock.json" 2>/dev/null ||
            { echo "$1 has no $src/package-lock.json" >&2; exit 2; }
        git archive "$1" "$src" | tar -C "$dir" -xf - --strip-components=2
    fi
    cp "$src"/.npmrc "$dir"/
    node "$src"/verify-lock.js "$dir"/package-lock.json "$age" >&2
    (cd "$dir" && npm ci "${npm_flags[@]}") >&2
    (cd "$dir" && ./node_modules/.bin/esbuild entry.js --bundle --format=iife \
        --target=es2022 --legal-comments=eof --alias:hammerjs=./hammer-stub.js \
        --outfile="$work/chartjs-bundle.$2.js" --log-level=warning)
}

old=$(label "${revs[0]:-HEAD}")
new=$(label "${revs[1]:-}")
if [ "$old" = "$new" ]; then
    echo "$old and $new are the same commit." >&2
    exit 0
fi
build "${revs[0]:-HEAD}" "$old"
build "${revs[1]:-}" "$new"

cd "$work"
status=0
git diff --no-index "${opts[@]}" -- "chartjs-bundle.$old.js" "chartjs-bundle.$new.js" || status=$?
case $status in
0) echo "The bundles at $old and $new are the same." >&2 ;;
1) ;;
*) exit "$status" ;;
esac
