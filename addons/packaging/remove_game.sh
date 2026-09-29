#!/bin/bash

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# remove CSS classes
perl -i -0777 -pe 's/\.btn-game\s*\{[^}]*\}\s*//g' "${PROJECT_ROOT}"/internal/web/static/style.css
perl -i -0777 -pe 's/\.btn-game:hover\s*\{[^}]*\}\s*//g' "${PROJECT_ROOT}"/internal/web/static/style.css

# remove dashboard link (comment + multi-line {{if .EasterEgg}} ... {{end}} block)
perl -i -0777 -pe 's/[ \t]*<!-- Game easter egg -->\n//g' "${PROJECT_ROOT}"/internal/web/static/index.html
perl -i -0777 -pe 's/[ \t]*\{\{if \.EasterEgg\}\}.*?\{\{end\}\}\n//gs' "${PROJECT_ROOT}"/internal/web/static/index.html

# make the easter_egg config option inert: drop the route registration so it has
# no effect even if left set in config (the option still parses, just does nothing)
perl -i -0777 -pe 's/\n[ \t]*if s\.global\.EasterEgg \{\n.*?\n[ \t]*\}\n/\n/s' "${PROJECT_ROOT}"/internal/web/server.go

# drop the now-dead game handler + game-path guard (golangci-lint `unused` would flag them)
perl -i -0777 -pe 's/\nfunc \(s \*Server\) handleGame\(w http\.ResponseWriter, r \*http\.Request\) \{\n.*?\n\}\n//s' "${PROJECT_ROOT}"/internal/web/server.go
perl -i -0777 -pe 's/\n[ \t]*if !s\.global\.EasterEgg && \(path == "game\.html".*?\n[ \t]*\}\n/\n/s' "${PROJECT_ROOT}"/internal/web/server.go

# remove game-specific tests that assert on now-removed game.js / game.html
perl -i -0777 -pe 's/\nfunc TestGameTemplateInjection\(t \*testing\.T\) \{.*?\n\}\n//s' "${PROJECT_ROOT}"/internal/web/server_test.go
perl -i -0777 -pe 's/\nfunc TestGameScoreURLTemplateAndCSP\(t \*testing\.T\) \{.*?\n\}\n//s' "${PROJECT_ROOT}"/internal/web/server_test.go
perl -i -0777 -pe 's/\nfunc TestInvalidGameScoreURLIsNotRendered\(t \*testing\.T\) \{.*?\n\}\n//s' "${PROJECT_ROOT}"/internal/web/server_test.go
perl -i -0777 -pe 's/\nfunc TestGameScoreSubmissionRequestPolicy\(t \*testing\.T\) \{.*?\n\}\n//s' "${PROJECT_ROOT}"/internal/web/server_test.go

# remove game files and the game-only font
rm -f "${PROJECT_ROOT}"/internal/web/static/game.*
rm -rf "${PROJECT_ROOT}"/internal/web/static/fonts/Press_Start_2P

# the substitutions above are silent no-ops once a pattern stops matching, so fail
# loudly if an upstream change left game references behind
if grep -n 'btn-game\|game\.html' "${PROJECT_ROOT}"/internal/web/static/index.html "${PROJECT_ROOT}"/internal/web/static/style.css \
    || grep -n 'handleGame\|game\.html\|game\.js' "${PROJECT_ROOT}"/internal/web/server.go "${PROJECT_ROOT}"/internal/web/server_test.go; then
    echo "remove_game.sh: game references remain (listed above); update the script" >&2
    exit 1
fi
