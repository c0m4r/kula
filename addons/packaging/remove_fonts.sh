#!/bin/bash

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# remove font-face CSS statements (style.css, plus game.css unless remove_game.sh ran first)
for css in "${PROJECT_ROOT}"/internal/web/static/*.css; do
    sed -i '/@font-face/,/}/d' "${css}"
done

# remove fonts
rm -rf "${PROJECT_ROOT}"/internal/web/static/fonts

# fail loudly if a stylesheet or page still points at the removed fonts
if grep -rn '@font-face\|fonts/' "${PROJECT_ROOT}"/internal/web/static --include='*.css' --include='*.html'; then
    echo "remove_fonts.sh: font references remain (listed above); update the script" >&2
    exit 1
fi
