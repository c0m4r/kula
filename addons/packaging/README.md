# Packaging - helper scripts

Run these against the source tree **before** building: they edit files under
`internal/web/`, which are embedded into the binary at build time. Both are optional,
independent and safe to run in either order. Each exits non-zero if references survive
its edits, which means an upstream change has outdated the script.

## remove_fonts.sh

Removes the bundled dashboard fonts (Inter, Press Start 2P) and their `@font-face` rules, so the
dashboard falls back to system-provided fonts.

## remove_game.sh

Disables and removes the embedded easter-egg game from the dashboard: the game page and assets,
the header button, the routes and handler, the game-only tests and the game's Press Start 2P
font. The `easter_egg` option still parses but does nothing. `game_score_url` also still parses
and, if set, still adds its origin to the CSP `connect-src`, so leave it empty.

# TODO

1. A packaging helper script that will replace chartjs bundle with 
chart.umd.min.js + chartjs-adapter-date-fns.bundle.min.js + chartjs-plugin-zoom.min.js trio
