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

# remove the game-specific test (asserts on now-removed game.js / game.html)
perl -i -0777 -pe 's/\nfunc TestGameTemplateInjection\(t \*testing\.T\) \{.*?\n\}\n//s' "${PROJECT_ROOT}"/internal/web/server_test.go

# remove game files
rm -f "${PROJECT_ROOT}"/internal/web/static/game.*
