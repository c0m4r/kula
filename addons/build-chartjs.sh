#!/usr/bin/env bash
# Rebuild the vendored Chart.js bundle from the pinned inputs in addons/chartjs/.
# Needs Node.js and npm; building Kula itself does not.
#
#   ./addons/build-chartjs.sh           rebuild from addons/chartjs/package-lock.json
#   ./addons/build-chartjs.sh --update  first bump chart.js, chartjs-plugin-zoom and
#                                       esbuild to their newest releases old enough
#                                       for min-release-age
#
# npm never runs install scripts here, only resolves releases at least
# min-release-age days old (addons/chartjs/.npmrc), and verify-lock.js checks
# every locked package before anything is installed.

set -euo pipefail

GREEN="\033[0;32m"
CYAN="\033[0;36m"
RESET="\033[0m"

cd "$(dirname "$0")/.."

src=addons/chartjs
out=internal/web/static/js/chartjs/chartjs-bundle.min.js
age=$(sed -n 's/^min-release-age=//p' "$src"/.npmrc)

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp "$src"/.npmrc "$src"/package.json "$src"/package-lock.json \
    "$src"/entry.js "$src"/date-adapter.js "$src"/hammer-stub.js "$work"/

# Command-line flags also override any npm_config_* environment variables.
npm_flags=(--ignore-scripts --min-release-age="$age" --no-audit --no-fund --loglevel=error)
if [ "${1:-}" = "--update" ]; then
    echo -e "${CYAN}Resolving the newest releases at least $age days old...${RESET}"
    (cd "$work" && npm install "${npm_flags[@]}" --package-lock-only --save-exact \
        chart.js@latest chartjs-plugin-zoom@latest esbuild@latest)
fi

echo -e "${CYAN}Verifying package-lock.json...${RESET}"
node "$src"/verify-lock.js "$work"/package-lock.json "$age"
(cd "$work" && npm ci "${npm_flags[@]}")

version() { node -p "require('$work/node_modules/$1/package.json').version"; }
banner="/*!
 * Kula Chart.js bundle, built by addons/build-chartjs.sh from addons/chartjs/
 * Chart.js v$(version chart.js) | MIT | https://www.chartjs.org
 * chartjs-plugin-zoom v$(version chartjs-plugin-zoom) | MIT | https://www.chartjs.org/chartjs-plugin-zoom/
 * @kurkle/color v$(version @kurkle/color) | MIT | https://github.com/kurkle/color
 */"

echo -e "${CYAN}Bundling Chart.js...${RESET}"
"$work"/node_modules/.bin/esbuild "$work"/entry.js --bundle --minify --format=iife \
    --target=es2022 --legal-comments=eof --alias:hammerjs="$work"/hammer-stub.js \
    --banner:js="$banner" --outfile="$out" --log-level=warning

echo -e "${GREEN}Wrote $out ($(wc -c <"$out") bytes)${RESET}"
if [ "${1:-}" = "--update" ]; then
    cp "$work"/package.json "$work"/package-lock.json "$src"/
    echo "Run ./addons/check.sh and ./addons/test-frontend-regressions.sh before committing."
fi
