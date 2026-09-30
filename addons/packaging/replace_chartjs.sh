#!/bin/bash
# replace_chartjs.sh - replace Kula's bundled esbuild Chart.js build with the
# three upstream dist files a distro ships:
#
#   chart.umd.min.js                       (chart.js core)
#   chartjs-adapter-date-fns.bundle.min.js (date adapter, bundles date-fns)
#   chartjs-plugin-zoom.min.js             (zoom plugin)
#
# The script edits the tree it lives in: it installs the trio into
# internal/web/static/js/chartjs/, loads them in dependency order from
# internal/web/static/index.html, removes the old esbuild bundle and its
# npm/esbuild build inputs (addons/chartjs/, which this script does not use),
# and updates the tests and fixtures that referenced the bundle. It exits
# non-zero when one of its substitutions no longer matches, which means an
# upstream change has outdated the script.
#
# Usage:
#   ./addons/packaging/replace_chartjs.sh [--from DIR]... [--download]
#
#   --from DIR   distro-provided copies (repeatable, or one colon-separated
#                list; $CHARTJS_DIST_DIR is honoured too). Searches DIR itself,
#                DIR/<pkg>, DIR/<pkg>/dist and DIR/dist, accepting the upstream
#                .min.js name or the unminified chart.umd.js /
#                chartjs-plugin-zoom.js / chartjs-adapter-date-fns.bundle.js.
#   --download   fetch the pinned releases below from registry.npmjs.org
#                (unpkg.com as fallback) and verify their sha256 before
#                installing. Also spelled --fetch.
#
# With neither flag the usual distro locations are searched:
#   /usr/share/javascript/<pkg>, /usr/share/nodejs/<pkg>/dist,
#   /usr/lib/node_modules/<pkg>/dist

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# Pinned upstream releases: --download fetches exactly these versions and
# verifies each dist file against these sha256 sums before installing it.
CHARTJS_VERSION="4.5.1"
CHARTJS_SHA256="48444a82d4edcb5bec0f1965faacdde18d9c17db3063d042abada2f705c9f54a"
ZOOM_VERSION="2.2.0"
ZOOM_SHA256="e4a088e5bab93be6ee47c939eeb9ebaa80e0b39156d4bdfd1af9c844be81b6c4"
ADAPTER_VERSION="3.0.0"
ADAPTER_SHA256="ea7ab30d26c38dcf1f2d26bb43e73a94537b58f1906f55e1a546dd09321b5615"

NPM_REGISTRY="https://registry.npmjs.org"
UNPKG="https://unpkg.com"

STATIC_DIR="${PROJECT_ROOT}/internal/web/static/js/chartjs"
INDEX_HTML="${PROJECT_ROOT}/internal/web/static/index.html"
PERF_HTML="${PROJECT_ROOT}/internal/web/testdata/history_performance.html"
BUNDLE_TEST="${PROJECT_ROOT}/internal/web/testdata/chart_bundle_test.mjs"
SERVER_TEST="${PROJECT_ROOT}/internal/web/server_test.go"
MINIFY_TEST="${PROJECT_ROOT}/internal/web/minify_test.go"

ROLES=(core adapter zoom)
declare -A RESOLVED=()
declare -A INSTALLED=()

die() {
    echo "replace_chartjs.sh: $*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
Usage: ./addons/packaging/replace_chartjs.sh [--from DIR]... [--download]

  --from DIR   distro-provided Chart.js dist files (repeatable, or one
               colon-separated list; $CHARTJS_DIST_DIR is honoured too)
  --download   fetch the pinned releases from the npm registry and verify
               their sha256 before installing (also spelled --fetch)
  -h, --help   show this help
EOF
}

pkg_name() {
    case "$1" in
    core) echo "chart.js" ;;
    adapter) echo "chartjs-adapter-date-fns" ;;
    zoom) echo "chartjs-plugin-zoom" ;;
    esac
}

pkg_version() {
    case "$1" in
    core) echo "${CHARTJS_VERSION}" ;;
    adapter) echo "${ADAPTER_VERSION}" ;;
    zoom) echo "${ZOOM_VERSION}" ;;
    esac
}

pkg_sha256() {
    case "$1" in
    core) echo "${CHARTJS_SHA256}" ;;
    adapter) echo "${ADAPTER_SHA256}" ;;
    zoom) echo "${ZOOM_SHA256}" ;;
    esac
}

pkg_min_name() {
    case "$1" in
    core) echo "chart.umd.min.js" ;;
    adapter) echo "chartjs-adapter-date-fns.bundle.min.js" ;;
    zoom) echo "chartjs-plugin-zoom.min.js" ;;
    esac
}

pkg_plain_name() {
    case "$1" in
    core) echo "chart.umd.js" ;;
    adapter) echo "chartjs-adapter-date-fns.bundle.js" ;;
    zoom) echo "chartjs-plugin-zoom.js" ;;
    esac
}

pkg_label() {
    case "$1" in
    core) echo "Chart.js core" ;;
    adapter) echo "date-fns time adapter" ;;
    zoom) echo "zoom plugin" ;;
    esac
}

# Structural checks for distro copies: they are not the pinned bytes, so only
# the library identity, size and expected globals/registration are checked.
sanity_check() {
    local role="$1" path="$2" size
    [ -f "${path}" ] || die "$(pkg_label "${role}"): ${path} is not a regular file"
    [ -s "${path}" ] || die "$(pkg_label "${role}"): ${path} is empty"
    size=$(wc -c <"${path}")
    # A minified error page, a source map or a stub is far smaller than these.
    case "${role}" in
    core) [ "${size}" -ge 50000 ] || die "$(pkg_label "${role}"): ${path} is only ${size} bytes; not a chart.js 4 dist file" ;;
    adapter) [ "${size}" -ge 10000 ] || die "$(pkg_label "${role}"): ${path} is only ${size} bytes; the date-fns bundle is required (not the bare chartjs-adapter-date-fns.js)" ;;
    zoom) [ "${size}" -ge 3000 ] || die "$(pkg_label "${role}"): ${path} is only ${size} bytes; not a chartjs-plugin-zoom dist file" ;;
    esac
    grep -q 'typeof exports' "${path}" || die "$(pkg_label "${role}"): ${path} has no UMD wrapper"
    case "${role}" in
    core)
        grep -q 'Chart\.js v' "${path}" || die "$(pkg_label "${role}"): ${path} has no 'Chart.js v' banner"
        grep -q '\.Chart *=' "${path}" || die "$(pkg_label "${role}"): ${path} does not define the global Chart"
        ;;
    adapter)
        # The unminified bundle carries no banner, so identify it by the
        # date-fns code it bundles and the adapter it registers; the size check
        # above rejects the bare adapter that expects date-fns to be loaded.
        grep -q 'date-fns' "${path}" || die "$(pkg_label "${role}"): ${path} has no date-fns reference"
        grep -q '_adapters\._date\.override' "${path}" || die "$(pkg_label "${role}"): ${path} does not register a Chart.js date adapter"
        ;;
    zoom)
        grep -q 'chartjs-plugin-zoom' "${path}" || die "$(pkg_label "${role}"): ${path} has no 'chartjs-plugin-zoom' banner"
        grep -q 'ChartZoom' "${path}" || die "$(pkg_label "${role}"): ${path} does not define the global ChartZoom"
        grep -q 'Chart\.register' "${path}" || die "$(pkg_label "${role}"): ${path} does not register itself on load"
        # Pinch still wants a global Hammer; the dashboard never enables it, so
        # a missing hammerjs must stay harmless (the file must be the one that
        # degrades gracefully, not a hammerjs-free fork).
        grep -q 'Hammer' "${path}" || die "$(pkg_label "${role}"): ${path} has no Hammer reference; not the upstream UMD file"
        ;;
    esac
}

set_resolved() {
    RESOLVED[$1]="$2"
    INSTALLED[$1]="$3"
}

# Searches the distro directories for one role; unminified files keep their
# upstream name so nothing is silently renamed.
resolve_from_dirs() {
    local role="$1" pkg min plain dir cand
    pkg=$(pkg_name "${role}")
    min=$(pkg_min_name "${role}")
    plain=$(pkg_plain_name "${role}")
    for dir in "${FROM_DIRS[@]}"; do
        for cand in "${dir}" "${dir}/${pkg}" "${dir}/${pkg}/dist" "${dir}/dist"; do
            [ -d "${cand}" ] || continue
            if [ -f "${cand}/${min}" ]; then
                set_resolved "${role}" "${cand}/${min}" "${min}"
                return 0
            fi
            if [ -f "${cand}/${plain}" ]; then
                set_resolved "${role}" "${cand}/${plain}" "${plain}"
                return 0
            fi
        done
    done
    return 1
}

verify_sha256() {
    local role="$1" path="$2" want actual
    want=$(pkg_sha256 "${role}")
    actual=$(sha256sum "${path}" | awk '{print $1}')
    if [ "${actual}" != "${want}" ]; then
        die "$(pkg_name "${role}")@$(pkg_version "${role}"): sha256 mismatch for ${path}
  expected ${want}
  got      ${actual}
The download is corrupted or not the pinned release; retry, or pass
--from DIR with a distro copy of the trio."
    fi
}

download_role() {
    local role="$1" pkg version min url target tgz
    pkg=$(pkg_name "${role}")
    version=$(pkg_version "${role}")
    min=$(pkg_min_name "${role}")
    target="${WORK}/download-${role}"
    mkdir -p "${target}"
    tgz="${target}/${pkg}-${version}.tgz"
    url="${NPM_REGISTRY}/${pkg}/-/${pkg}-${version}.tgz"
    if curl -fsSL --retry 3 --connect-timeout 20 -o "${tgz}" "${url}"; then
        tar -xzf "${tgz}" -C "${target}" "package/dist/${min}" ||
            die "${pkg}@${version}: tarball does not contain package/dist/${min}"
        RESOLVED[${role}]="${target}/package/dist/${min}"
    else
        echo "  npm registry fetch failed for ${pkg}@${version}; trying unpkg.com" >&2
        curl -fsSL --retry 3 --connect-timeout 20 -o "${target}/${min}" "${UNPKG}/${pkg}@${version}/dist/${min}" ||
            die "cannot download ${pkg}@${version} from ${NPM_REGISTRY} or ${UNPKG}"
        RESOLVED[${role}]="${target}/${min}"
    fi
    verify_sha256 "${role}" "${RESOLVED[${role}]}"
    sanity_check "${role}" "${RESOLVED[${role}]}"
    INSTALLED[${role}]="${min}"
    echo "  ${pkg}@${version}: downloaded and sha256 verified"
}

# Replaces the literal block in OLD_FILE with the literal block in NEW_FILE.
# Exit 0 = replaced, 1 = target already holds the new block, 2 = neither found
# (an upstream change outdated the script). PATCH_DRY=1 only reports.
patch_state() {
    OLD_FILE="$1" NEW_FILE="$2" TARGET_FILE="$3" perl -e '
        local $/;
        sub slurp {
            my ($file) = @_;
            open my $fh, "<", $file or die "cannot read $file: $!";
            my $text = <$fh>;
            close $fh;
            return $text;
        }
        my $old = slurp($ENV{OLD_FILE});
        my $new = slurp($ENV{NEW_FILE});
        my $src = slurp($ENV{TARGET_FILE});
        my $at = index($src, $old);
        my $state = $at >= 0 ? 0 : (index($src, $new) >= 0 ? 1 : 2);
        if ($state == 0 && !$ENV{PATCH_DRY}) {
            substr($src, $at, length($old)) = $new;
            my $tmp = "$ENV{TARGET_FILE}.tmp$$";
            open my $out, ">", $tmp or die "cannot write $tmp: $!";
            print $out $src;
            close $out;
            rename $tmp, $ENV{TARGET_FILE} or die "cannot replace $ENV{TARGET_FILE}: $!";
        }
        exit $state;
    '
}

# Phase 1: refuse to touch the tree unless every substitution can be applied.
check_patch() {
    local description="$1" state=0
    PATCH_DRY=1 patch_state "$2" "$3" "$4" || state=$?
    case "${state}" in
    0) ;;
    1) echo "  ${description}: already up to date" ;;
    *) die "${4}: expected upstream block not found; update the script" ;;
    esac
}

# Phase 2: apply the substitution.
apply_patch() {
    local description="$1" state=0
    patch_state "$2" "$3" "$4" || state=$?
    case "${state}" in
    0) echo "  patched ${description}" ;;
    1) echo "  ${description}: already up to date" ;;
    *) die "${4}: expected upstream block not found; update the script" ;;
    esac
}

# Builds the replacement snippets for the actual installed file names.
make_snippets() {
    local core adapter zoom name
    core="${INSTALLED[core]}"
    adapter="${INSTALLED[adapter]}"
    zoom="${INSTALLED[zoom]}"

    {
        printf '    <!-- Chart.js trio from the distro: core, date adapter, zoom plugin -->\n'
        for name in "${core}" "${adapter}" "${zoom}"; do
            printf '    <script src="js/chartjs/%s" integrity="{{sri `js/chartjs/%s`}}"\n        nonce="{{.Nonce}}" crossorigin="anonymous"></script>\n' "${name}" "${name}"
        done
    } >"${WORK}/index-new"
    printf '    <script src="js/chartjs/chartjs-bundle.min.js" integrity="{{sri `js/chartjs/chartjs-bundle.min.js`}}"\n        nonce="{{.Nonce}}" crossorigin="anonymous"></script>\n' >"${WORK}/index-old"

    {
        for name in "${core}" "${adapter}" "${zoom}"; do
            printf '    <script src="../static/js/chartjs/%s"></script>\n' "${name}"
        done
    } >"${WORK}/perf-new"
    printf '    <script src="../static/js/chartjs/chartjs-bundle.min.js"></script>\n' >"${WORK}/perf-old"

    {
        printf '\tfor _, chartjs := range []string{\n'
        for name in "${core}" "${adapter}" "${zoom}"; do
            printf '\t\t"js/chartjs/%s",\n' "${name}"
        done
        printf '\t} {\n'
        printf '\t\thash := s.sriHashes[chartjs]\n'
        printf '\t\tif hash == "" || !strings.Contains(body, `integrity="`+hash+`"`) {\n'
        printf '\t\t\tt.Errorf("HTML body missing the SRI for %%s", chartjs)\n'
        printf '\t\t}\n'
        printf '\t}\n'
    } >"${WORK}/server-test-new"
    printf '\tif bundle := s.sriHashes["js/chartjs/chartjs-bundle.min.js"]; bundle == "" || !strings.Contains(body, `integrity="`+bundle+`"`) {\n\t\tt.Error("HTML body missing the Chart.js bundle SRI")\n\t}\n' >"${WORK}/server-test-old"

    printf 'js/chartjs/chartjs-bundle.min.js' >"${WORK}/minify-old"
    printf 'js/chartjs/%s' "${core}" >"${WORK}/minify-new"
}

# The old bundle test unit-tested addons/chartjs/date-adapter.js; the trio is
# upstream code, so the replacement checks that the stock files register what
# the dashboard uses and that the date-fns adapter still drives local-time
# ticks across a DST boundary.
write_bundle_test() {
    if [ -f "${BUNDLE_TEST}" ] && ! grep -q 'chartjs-bundle\.min\.js\|replace_chartjs\.sh' "${BUNDLE_TEST}"; then
        die "${BUNDLE_TEST} does not look like the Chart.js bundle test; update the script"
    fi
    cat >"${BUNDLE_TEST}" <<'CHARTJS_TEST_EOF'
// Checks the upstream Chart.js trio that addons/packaging/replace_chartjs.sh
// installs in place of Kula's esbuild bundle: the stock chart.js UMD dist
// file, chartjs-adapter-date-fns with date-fns bundled, and chartjs-plugin-zoom.
// They register everything upstream ships, a superset of what the dashboard
// uses. Calendar math is local time, so pin a DST zone.
process.env.TZ = 'America/New_York';

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const local = (...parts) => new Date(...parts).getTime();
const HOUR = 3600000;

// Dependency order matters: the adapter overrides Chart's date adapter, and the
// zoom plugin registers itself against the global Chart while it loads.
const TRIO = [
    '@CHART_CORE@',
    '@CHART_ADAPTER@',
    '@CHART_ZOOM@',
];

let loaded;
function loadTrio() {
    loaded ??= (async () => {
        for (const name of TRIO) {
            const source = await readFile(new URL(`../static/js/chartjs/${name}`, import.meta.url), 'utf8');
            vm.runInThisContext(source, { filename: name });
        }
        return globalThis.Chart;
    })();
    return loaded;
}

// Enough of CanvasRenderingContext2D for layout: text is 6px per character.
function stubCanvas(width, height) {
    const canvas = { width, height, style: {} };
    const values = { canvas };
    canvas.getContext = () => new Proxy(values, {
        get: (target, key) => (key in target ? target[key]
            : key === 'measureText' ? text => ({ width: String(text).length * 6 }) : () => {}),
        set: (target, key, value) => { target[key] = value; return true; },
    });
    return canvas;
}

test('the trio registers the chart surface the dashboard uses', async () => {
    const Chart = await loadTrio();
    const names = registry => Object.keys(registry.items);

    assert.equal(typeof Chart.getChart, 'function');
    assert.equal(typeof Chart.defaults.font, 'object');
    for (const id of ['line']) {
        assert.ok(names(Chart.registry.controllers).includes(id), `line controller`);
    }
    for (const id of ['line', 'point']) {
        assert.ok(names(Chart.registry.elements).includes(id), `${id} element`);
    }
    for (const id of ['linear', 'time']) {
        assert.ok(names(Chart.registry.scales).includes(id), `${id} scale`);
    }
    for (const id of ['legend', 'tooltip', 'zoom']) {
        assert.ok(names(Chart.registry.plugins).includes(id), `${id} plugin`);
    }
    // state.js extends the shared positioner table and reads the tooltip plugin.
    assert.equal(typeof Chart.Tooltip.positioners, 'object');
    assert.equal(typeof Chart.registry.plugins.get('tooltip'), 'object');
    Chart.Tooltip.positioners.awayFromCursor = () => ({ x: 0, y: 0 });
    assert.equal(typeof Chart.Tooltip.positioners.awayFromCursor, 'function');
});

test('the date-fns adapter drives local-time ticks across the DST gap', async () => {
    const Chart = await loadTrio();
    assert.equal(Chart._adapters._date.prototype._id, 'date-fns');

    // Six hours across the spring-forward gap: ticks stay on local hours.
    const start = local(2024, 2, 10, 0);
    const chart = new Chart(stubCanvas(800, 300), {
        type: 'line',
        data: { datasets: [{ data: [{ x: start, y: 1 }, { x: start + 6 * HOUR, y: 2 }] }] },
        options: {
            responsive: false,
            animation: false,
            parsing: false,
            scales: { x: { type: 'time', time: { unit: 'hour' } } },
        },
    });
    const ticks = chart.scales.x.ticks;
    assert.deepEqual(ticks.map(tick => new Date(tick.value).getHours()), [0, 1, 3, 4, 5, 6, 7]);
    assert.ok(ticks.every(tick => tick.value % (15 * 60000) === 0));
    assert.ok(ticks.every(tick => typeof tick.label === 'string' && tick.label.length > 0));
    assert.equal(typeof chart.isZoomingOrPanning, 'function');
    chart.destroy();
});

test('the zoom plugin registers without hammerjs', async () => {
    const Chart = await loadTrio();
    // Only pinch needs Hammer, and the dashboard keeps pinch disabled: pan and
    // zoom use Pointer Events, so loading without hammerjs must stay harmless.
    assert.equal(globalThis.Hammer, undefined);
    assert.equal(globalThis.ChartZoom.id, 'zoom');
    assert.equal(typeof Chart.registry.plugins.get('zoom'), 'object');
});
CHARTJS_TEST_EOF

    CHART_CORE="${INSTALLED[core]}" CHART_ADAPTER="${INSTALLED[adapter]}" CHART_ZOOM="${INSTALLED[zoom]}" \
        perl -0777 -i -pe '
            s/\@CHART_CORE\@/$ENV{CHART_CORE}/g;
            s/\@CHART_ADAPTER\@/$ENV{CHART_ADAPTER}/g;
            s/\@CHART_ZOOM\@/$ENV{CHART_ZOOM}/g;
        ' "${BUNDLE_TEST}"
}

# Fail loudly if an upstream change made one of the substitutions a silent
# no-op or left a reference to the removed bundle behind.
verify_tree() {
    local leftovers count role
    if leftovers=$(grep -rn 'chartjs-bundle\.min\.js' "${PROJECT_ROOT}/internal/web" 2>/dev/null); then
        echo "${leftovers}" >&2
        die "references to the removed esbuild bundle remain (listed above); update the script"
    fi
    if leftovers=$(grep -rn 'addons/chartjs' "${PROJECT_ROOT}/internal/web" 2>/dev/null); then
        echo "${leftovers}" >&2
        die "references to the removed build inputs remain (listed above); update the script"
    fi
    for role in "${ROLES[@]}"; do
        [ -f "${STATIC_DIR}/${INSTALLED[$role]}" ] || die "${STATIC_DIR}/${INSTALLED[$role]} is missing"
        grep -q "js/chartjs/${INSTALLED[$role]}" "${INDEX_HTML}" ||
            die "internal/web/static/index.html does not load ${INSTALLED[$role]}; update the script"
    done
    count=$(grep -c 'js/chartjs/' "${INDEX_HTML}" || true)
    [ "${count}" = "3" ] || die "internal/web/static/index.html has ${count} js/chartjs/ references, want exactly 3; update the script"
    [ ! -e "${STATIC_DIR}/chartjs-bundle.min.js" ] || die "could not remove ${STATIC_DIR}/chartjs-bundle.min.js"
    [ ! -e "${PROJECT_ROOT}/addons/chartjs" ] || die "could not remove ${PROJECT_ROOT}/addons/chartjs"
}

FROM_DIRS=()
DOWNLOAD=0
while [ $# -gt 0 ]; do
    case "$1" in
    --from)
        [ $# -ge 2 ] || die "--from needs a directory"
        IFS=':' read -r -a from_parts <<<"$2"
        FROM_DIRS+=("${from_parts[@]}")
        shift 2
        ;;
    --from=*)
        IFS=':' read -r -a from_parts <<<"${1#--from=}"
        FROM_DIRS+=("${from_parts[@]}")
        shift
        ;;
    --download | --fetch)
        DOWNLOAD=1
        shift
        ;;
    -h | --help)
        usage
        exit 0
        ;;
    *)
        die "unknown argument: $1 (try --help)"
        ;;
    esac
done

if [ -n "${CHARTJS_DIST_DIR:-}" ]; then
    IFS=':' read -r -a from_parts <<<"${CHARTJS_DIST_DIR}"
    FROM_DIRS+=("${from_parts[@]}")
fi

[ -f "${INDEX_HTML}" ] || die "${INDEX_HTML} not found; run this from the Kula source tree"

if [ "${DOWNLOAD}" = 1 ]; then
    command -v curl >/dev/null || die "--download needs curl"
    command -v tar >/dev/null || die "--download needs tar"
fi

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

echo "replace_chartjs.sh: installing the upstream Chart.js trio into internal/web/static/js/chartjs"

if [ ${#FROM_DIRS[@]} -gt 0 ]; then
    for role in "${ROLES[@]}"; do
        resolve_from_dirs "${role}" || true
    done
elif [ "${DOWNLOAD}" = 1 ]; then
    :
else
    # No source given: the usual distro locations, searched in this order.
    FROM_DIRS=(/usr/share/javascript /usr/share/nodejs /usr/lib/node_modules)
    for role in "${ROLES[@]}"; do
        resolve_from_dirs "${role}" || true
    done
fi

for role in "${ROLES[@]}"; do
    if [ -z "${RESOLVED[$role]:-}" ]; then
        continue
    fi
    # Distro copies are not the pinned bytes; check their structure here.
    sanity_check "${role}" "${RESOLVED[$role]}"
done

for role in "${ROLES[@]}"; do
    if [ -z "${RESOLVED[$role]:-}" ]; then
        if [ "${DOWNLOAD}" = 1 ]; then
            download_role "${role}"
        else
            die "$(pkg_label "${role}") ($(pkg_min_name "${role}") or $(pkg_plain_name "${role}")) not found in: ${FROM_DIRS[*]}
Pass --from DIR with distro-provided copies, --download to fetch the pinned
releases ($(pkg_name "${role}")@$(pkg_version "${role}")), or set \$CHARTJS_DIST_DIR."
        fi
    fi
done

for role in "${ROLES[@]}"; do
    echo "using ${RESOLVED[$role]} for internal/web/static/js/chartjs/${INSTALLED[$role]}"
done

make_snippets

# Refuse to touch the tree unless every substitution can be applied.
check_patch "internal/web/static/index.html" "${WORK}/index-old" "${WORK}/index-new" "${INDEX_HTML}"
check_patch "internal/web/testdata/history_performance.html" "${WORK}/perf-old" "${WORK}/perf-new" "${PERF_HTML}"
check_patch "internal/web/server_test.go" "${WORK}/server-test-old" "${WORK}/server-test-new" "${SERVER_TEST}"
check_patch "internal/web/minify_test.go" "${WORK}/minify-old" "${WORK}/minify-new" "${MINIFY_TEST}"

mkdir -p "${STATIC_DIR}"
for role in "${ROLES[@]}"; do
    rm -f "${STATIC_DIR}/$(pkg_min_name "${role}")" "${STATIC_DIR}/$(pkg_plain_name "${role}")"
    cp "${RESOLVED[$role]}" "${STATIC_DIR}/${INSTALLED[$role]}"
done

rm -f "${STATIC_DIR}/chartjs-bundle.min.js"
echo "  removed internal/web/static/js/chartjs/chartjs-bundle.min.js"
rm -rf "${PROJECT_ROOT}/addons/chartjs"
echo "  removed addons/chartjs/ (npm + esbuild build inputs; this script needs neither,"
echo "  and addons/build-chartjs.sh can no longer run on this tree)"

apply_patch "internal/web/static/index.html (trio in dependency order)" "${WORK}/index-old" "${WORK}/index-new" "${INDEX_HTML}"
apply_patch "internal/web/testdata/history_performance.html" "${WORK}/perf-old" "${WORK}/perf-new" "${PERF_HTML}"
apply_patch "internal/web/server_test.go (all three SRI hashes)" "${WORK}/server-test-old" "${WORK}/server-test-new" "${SERVER_TEST}"
apply_patch "internal/web/minify_test.go" "${WORK}/minify-old" "${WORK}/minify-new" "${MINIFY_TEST}"
write_bundle_test
echo "  rewrote internal/web/testdata/chart_bundle_test.mjs for the trio"

verify_tree
echo "replace_chartjs.sh: done - run ./addons/check.sh on this tree before packaging"
