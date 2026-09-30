# Packaging - helper scripts

Run these against the source tree **before** building: they edit files under
`internal/web/`, which are embedded into the binary at build time. All three are optional,
independent and safe to run in any order. Each exits non-zero if references survive its
edits, which means an upstream change has outdated the script.

## remove_fonts.sh | status: stable

Removes the bundled dashboard fonts (Inter, Press Start 2P) and their `@font-face` rules, so the
dashboard falls back to system-provided fonts.

## remove_game.sh | status: stable

Disables and removes the embedded easter-egg game from the dashboard: the game page and assets,
the header button, the routes and handler, the game-only tests and the game's Press Start 2P
font. The `easter_egg` option still parses but does nothing. `game_score_url` also still parses
and, if set, still adds its origin to the CSP `connect-src`, so leave it empty.

## replace_chartjs.sh | status: testing

Replaces the vendored esbuild Chart.js bundle (`js/chartjs/chartjs-bundle.min.js`) with the
three upstream dist files a distro provides: `chart.umd.min.js` (chart.js),
`chartjs-adapter-date-fns.bundle.min.js` (date-fns time adapter) and
`chartjs-plugin-zoom.min.js` (zoom plugin).

```bash
./addons/packaging/replace_chartjs.sh --from /usr/share/javascript
./addons/packaging/replace_chartjs.sh --download
```

`--from DIR` (repeatable, or one colon-separated list; `$CHARTJS_DIST_DIR` works too) searches
`DIR`, `DIR/<pkg>`, `DIR/<pkg>/dist` and `DIR/dist`, accepting either the `.min.js` upstream
name or the unminified `chart.umd.js` / `chartjs-plugin-zoom.js` /
`chartjs-adapter-date-fns.bundle.js`. Distro copies are checked for the library banner, a
plausible size and the expected global or registration, not for byte equality; unminified
files keep their upstream name. With neither `--from` nor `--download` the usual distro
locations are searched: `/usr/share/javascript/<pkg>`, `/usr/share/nodejs/<pkg>/dist` and
`/usr/lib/node_modules/<pkg>/dist`. `--download` (also `--fetch`) fetches the pinned releases
from the npm registry, with unpkg.com as fallback, and verifies each file's sha256 before
installing it.

The script installs the trio in `internal/web/static/js/chartjs/` and loads it from
`internal/web/static/index.html` in dependency order (core, adapter, zoom), each file keeping
its SRI and nonce attributes. It also updates `internal/web/server_test.go`,
`internal/web/minify_test.go`, `internal/web/testdata/history_performance.html` and
`internal/web/testdata/chart_bundle_test.mjs` for the trio, then removes the old bundle and
`addons/chartjs/`, the npm/esbuild build inputs it does not use (`addons/build-chartjs.sh` can
no longer run on that tree). Unlike the bundle, the upstream files register everything
Chart.js ships — all controllers, elements, scales and plugins, a superset of what the
dashboard uses — with the date-fns adapter instead of the native-`Date` one; pinch-zoom stays
inert without hammerjs, which is fine because Kula pans and zooms with Pointer Events.
