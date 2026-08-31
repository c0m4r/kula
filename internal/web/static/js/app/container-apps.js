/* ============================================================
   container-apps.js — Multi-series container charts (one chart
   per metric type) + multi-select application filter.
   ============================================================ */
'use strict';
import { state, colors } from './state.js';
import { formatBytesShort } from './utils.js';
import { createTimeSeriesChart } from './charts-init.js';
import { i18n } from './i18n.js';
import { pinSectionHeadForCards } from './section-utils.js';

// Palette for assigning a stable color per container/app.
const CONTAINER_COLOR_LIST = [
    colors.blue, colors.green, colors.orange, colors.purple, colors.cyan,
    colors.red, colors.yellow, colors.pink, colors.teal, colors.lime,
];

// Dash patterns disambiguate series once the palette wraps around, so the
// 11th container is still distinguishable from the 1st.
const CONTAINER_DASH_LIST = [[], [6, 3], [2, 2], [8, 3, 2, 3]];

const CONTAINER_CARD_SELECTOR = '[data-container-metric]';

// Metric chart definitions: one chart per metric type, one series per app.
// Titles reuse existing i18n keys; qualifiers are unit-free semantic keys
// (rx/tx/disk_read/disk_write) so no locale needs suffix stripping.
const CONTAINER_METRICS = [
    {
        key: 'cpu',
        cardId: 'card-containers-cpu',
        chartId: 'chart-containers-cpu',
        subtitleId: 'containers-cpu-subtitle',
        i18nKeys: ['cpu_usage'],
        order: 20,
        yConfig: { beginAtZero: true, ticks: { callback: v => v + '%' } },
        field: 'cpu_pct',
    },
    {
        key: 'mem',
        cardId: 'card-containers-mem',
        chartId: 'chart-containers-mem',
        subtitleId: 'containers-mem-subtitle',
        i18nKeys: ['memory_usage'],
        order: 21,
        yConfig: { beginAtZero: true, ticks: { callback: v => formatBytesShort(v) } },
        field: 'mem_used',
    },
    {
        key: 'net_rx',
        cardId: 'card-containers-net-rx',
        chartId: 'chart-containers-net-rx',
        subtitleId: 'containers-net-rx-subtitle',
        // System has a single "Network Throughput" chart with RX/TX series;
        // we split direction into two cards and keep the same base label.
        i18nKeys: ['network_throughput', 'rx'],
        order: 22,
        yConfig: { beginAtZero: true, ticks: { callback: v => formatBytesShort(v) + '/s' } },
        field: 'net_rx_bps',
    },
    {
        key: 'net_tx',
        cardId: 'card-containers-net-tx',
        chartId: 'chart-containers-net-tx',
        subtitleId: 'containers-net-tx-subtitle',
        i18nKeys: ['network_throughput', 'tx'],
        order: 23,
        yConfig: { beginAtZero: true, ticks: { callback: v => formatBytesShort(v) + '/s' } },
        field: 'net_tx_bps',
    },
    {
        key: 'disk_r',
        cardId: 'card-containers-disk-r',
        chartId: 'chart-containers-disk-r',
        subtitleId: 'containers-disk-r-subtitle',
        i18nKeys: ['disk_io', 'disk_read'],
        order: 24,
        yConfig: { beginAtZero: true, ticks: { callback: v => formatBytesShort(v) + '/s' } },
        field: 'disk_r_bps',
    },
    {
        key: 'disk_w',
        cardId: 'card-containers-disk-w',
        chartId: 'chart-containers-disk-w',
        subtitleId: 'containers-disk-w-subtitle',
        i18nKeys: ['disk_io', 'disk_write'],
        order: 25,
        yConfig: { beginAtZero: true, ticks: { callback: v => formatBytesShort(v) + '/s' } },
        field: 'disk_w_bps',
    },
];

/** Build a chart title from existing i18n keys (base, or "base (qualifier)"). */
function metricTitle(m) {
    const keys = m.i18nKeys || [];
    if (keys.length === 0) return m.key;
    if (keys.length === 1) return i18n.t(keys[0]);
    return `${i18n.t(keys[0])} (${i18n.t(keys[1])})`;
}

function applyMetricCardTitle(m) {
    const h3 = document.querySelector(`#${m.cardId} h3`);
    if (!h3) return;
    h3.textContent = metricTitle(m);
    // Single-key titles can also ride the global data-i18n pass.
    if (m.i18nKeys?.length === 1) {
        h3.setAttribute('data-i18n', m.i18nKeys[0]);
    } else {
        h3.removeAttribute('data-i18n');
    }
}

function retranslateContainerUI() {
    for (const m of CONTAINER_METRICS) applyMetricCardTitle(m);
    const btnLabel = document.querySelector('.container-app-filter-btn-label');
    if (btnLabel) btnLabel.textContent = i18n.t('applications');
    const panel = document.getElementById('container-app-filter-panel');
    const selAll = panel?.querySelector('[data-action="select-all"]');
    const deselAll = panel?.querySelector('[data-action="deselect-all"]');
    if (selAll) selAll.textContent = i18n.t('select_all');
    if (deselAll) deselAll.textContent = i18n.t('deselect_all');
    const legend = document.getElementById('container-app-legend');
    if (legend) legend.setAttribute('aria-label', i18n.t('container_legend'));
    // Locale change invalidates rendered text everywhere.
    renderState.apps = null;
    renderState.selection = null;
    updateFilterButtonLabel();
    renderSharedLegend();
    renderFilterList();
    updateMetricSubtitles();
    updateSearchTerms();
}

// Re-apply titles when the user switches language.
if (typeof document !== 'undefined') {
    document.addEventListener('kula-i18n-changed', retranslateContainerUI);
}

const FILTER_STORAGE_KEY = 'kula_container_filter';

// Signatures of the last rendered app set / selection. Re-rendering the legend
// and the filter list is DOM-heavy, and addContainerSample runs once per sample
// (and up to maxBufferSize times during redrawChartsFromBuffer), so both are
// rebuilt only when their inputs actually change.
const renderState = { apps: null, selection: null };

function appsSignature() {
    return Object.values(state.containerApps || {})
        .map(a => `${a.key}${a.label}${a.color}${a.dashIndex}`)
        .sort()
        .join('');
}

function selectionSignature() {
    ensureFilterState();
    return [...state.containerExcluded].sort().join('');
}

// Stable chart/app key for a container sample. Prefer the human-readable name so
// history survives recreate (Docker assigns a new ID each time). Fall back to
// id when name is empty (cgroups-only discovery has no names).
function containerSeriesKey(ct) {
    const raw = (ct.name && String(ct.name).trim()) || ct.id || 'unknown';
    return 'container_' + encodeURIComponent(String(raw));
}

function containerDisplayName(ct) {
    return (ct.name && String(ct.name).trim()) || ct.id || 'unknown';
}

function hexToRgba(hex, alpha) {
    const h = String(hex).replace('#', '');
    if (h.length !== 6) return `rgba(59, 130, 246, ${alpha})`;
    const r = parseInt(h.slice(0, 2), 16);
    const g = parseInt(h.slice(2, 4), 16);
    const b = parseInt(h.slice(4, 6), 16);
    return `rgba(${r}, ${g}, ${b}, ${alpha})`;
}

// ---- Filter state: exclusions (not selections) ----
// localStorage shape: { "excluded": ["container_foo", ...] }
// Empty / missing → all selected. New containers are never in excluded → on by default.

function loadExcluded() {
    try {
        const raw = localStorage.getItem(FILTER_STORAGE_KEY);
        if (!raw) return new Set();
        const parsed = JSON.parse(raw);
        if (parsed && Array.isArray(parsed.excluded)) {
            return new Set(parsed.excluded);
        }
        // A bare array is the pre-release selected-keys format. The full container
        // set is unknown at load time, so it cannot be inverted into exclusions;
        // drop it and start with everything selected.
        if (Array.isArray(parsed)) {
            localStorage.removeItem(FILTER_STORAGE_KEY);
        }
    } catch (_) { /* corrupt value — fall through to "all selected" */ }
    return new Set();
}

function saveExcluded() {
    ensureFilterState();
    const excluded = [...state.containerExcluded];
    if (excluded.length === 0) {
        localStorage.removeItem(FILTER_STORAGE_KEY);
    } else {
        localStorage.setItem(FILTER_STORAGE_KEY, JSON.stringify({ excluded }));
    }
}

function ensureFilterState() {
    if (!(state.containerExcluded instanceof Set)) {
        state.containerExcluded = loadExcluded();
    }
}

function isContainerSelected(key) {
    ensureFilterState();
    return !state.containerExcluded.has(key);
}

function setContainerSelected(key, selected) {
    ensureFilterState();
    if (selected) state.containerExcluded.delete(key);
    else state.containerExcluded.add(key);
}

function selectAllContainers() {
    ensureFilterState();
    state.containerExcluded.clear();
    saveExcluded();
}

function deselectAllContainers() {
    ensureFilterState();
    state.containerExcluded = new Set(Object.keys(state.containerApps || {}));
    saveExcluded();
}

function ensureContainerApp(key, label, id, tsMs) {
    if (!state.containerApps) state.containerApps = {};
    const existing = state.containerApps[key];
    if (existing) {
        existing.label = label;
        if (id) existing.id = id;
        existing.lastSeen = tsMs;
        return existing;
    }
    // First unused slot, else wrap around and vary the dash pattern so wrapped
    // colors stay distinguishable.
    const used = new Set(Object.values(state.containerApps).map(a => a.slot));
    let slot = 0;
    while (used.has(slot)) slot++;
    const colorIndex = slot % CONTAINER_COLOR_LIST.length;
    const dashIndex = Math.floor(slot / CONTAINER_COLOR_LIST.length) % CONTAINER_DASH_LIST.length;
    state.containerApps[key] = {
        key,
        label,
        id: id || '',
        slot,
        color: CONTAINER_COLOR_LIST[colorIndex],
        dash: CONTAINER_DASH_LIST[dashIndex],
        dashIndex,
        lastSeen: tsMs,
    };
    // New apps are selected by default: do NOT add to containerExcluded.
    ensureFilterState();
    return state.containerApps[key];
}

/**
 * Drop containers that have been gone longer than the displayed time window.
 * Without this, every dead container keeps a dataset (fed a null point per
 * sample) plus a legend and filter entry, forever.
 */
function pruneDeadContainers(liveKeys, tsMs) {
    // A custom/unbounded range has no cutoff to prune against.
    if (typeof state.timeRange !== 'number' || !(state.timeRange > 0)) return;
    const cutoff = tsMs - state.timeRange * 1000;
    const doomed = [];
    for (const [key, app] of Object.entries(state.containerApps || {})) {
        if (liveKeys.has(key)) continue;
        if (typeof app.lastSeen !== 'number' || app.lastSeen < cutoff) doomed.push(key);
    }
    if (doomed.length === 0) return;

    const dead = new Set(doomed);
    for (const m of CONTAINER_METRICS) {
        const chart = state.containerCharts?.[m.key];
        if (!chart) continue;
        chart.data.datasets = chart.data.datasets.filter(ds => !dead.has(ds.containerKey));
    }
    for (const key of doomed) delete state.containerApps[key];
}

// ---- Charts ----

function ensureMetricCharts(createAppChartCard) {
    if (!state.containerCharts) state.containerCharts = {};
    for (const m of CONTAINER_METRICS) {
        if (state.containerCharts[m.key]) continue;
        createAppChartCard(m.cardId, m.chartId, m.subtitleId, metricTitle(m), m.order);
        applyMetricCardTitle(m);
        const chart = createTimeSeriesChart(m.chartId, [], m.yConfig);
        if (chart) {
            // Shared legend under Applications; no per-card Chart.js legend.
            if (chart.options.plugins?.legend) chart.options.plugins.legend.display = false;
            // Multi-series with null gaps must not use normalized mode.
            chart.options.normalized = false;
        }
        state.containerCharts[m.key] = chart;
        const card = document.getElementById(m.cardId);
        if (card) card.dataset.containerMetric = m.key;
    }
}

/** Align a new dataset length to existing series on the same chart (null backfill). */
function alignedNullPrefix(chart, ts) {
    let len = 0;
    for (const ds of chart.data.datasets) {
        if (Array.isArray(ds.data) && ds.data.length > len) len = ds.data.length;
    }
    if (len === 0) return [];
    // Prefer real timestamps from a peer dataset when available.
    const peer = chart.data.datasets.find(d => Array.isArray(d.data) && d.data.length === len);
    const out = new Array(len);
    for (let i = 0; i < len; i++) {
        const pt = peer?.data[i];
        const x = pt && typeof pt === 'object' && pt.x != null ? pt.x : ts;
        out[i] = { x, y: null };
    }
    return out;
}

function ensureSeriesForApp(app, ts) {
    for (const m of CONTAINER_METRICS) {
        const chart = state.containerCharts[m.key];
        if (!chart) continue;
        let ds = chart.data.datasets.find(d => d.containerKey === app.key);
        if (ds) {
            ds.label = app.label;
            ds.borderColor = app.color;
            ds.backgroundColor = hexToRgba(app.color, 0.12);
            ds.borderDash = app.dash;
            ds.hidden = !isContainerSelected(app.key);
            continue;
        }
        chart.data.datasets.push({
            containerKey: app.key,
            label: app.label,
            borderColor: app.color,
            backgroundColor: hexToRgba(app.color, 0.12),
            borderDash: app.dash,
            fill: false,
            data: alignedNullPrefix(chart, ts),
            pointRadius: 0,
            borderWidth: 1.5,
            tension: 0.2,
            hidden: !isContainerSelected(app.key),
        });
    }
}

/**
 * Append one tick to every known series on every metric chart.
 * Present containers get real values; others get {x:ts, y:null} so dataset
 * lengths stay aligned for Chart.js interaction.mode: 'index'.
 * Chart redraws are left to syncContainerMetricsUI so each chart repaints once.
 */
function appendAlignedTick(ts, liveByKey, point) {
    for (const m of CONTAINER_METRICS) {
        const chart = state.containerCharts?.[m.key];
        if (!chart) continue;
        for (const ds of chart.data.datasets) {
            const key = ds.containerKey;
            if (!key) continue;
            const ct = liveByKey[key];
            ds.data.push(ct ? point(ct[m.field] || 0) : { x: ts, y: null });
        }
    }
}

/**
 * Single source of truth for container metric card / filter / legend visibility.
 * @param {Set<string>} liveKeys containers present in the current sample (may be empty)
 */
function syncContainerMetricsUI(liveKeys) {
    const hasLive = liveKeys && liveKeys.size > 0;
    const anySelected = Object.keys(state.containerApps || {}).some(k => isContainerSelected(k));
    const showCards = hasLive && anySelected;

    for (const m of CONTAINER_METRICS) {
        const chart = state.containerCharts?.[m.key];
        const card = document.getElementById(m.cardId);
        if (chart) {
            for (const ds of chart.data.datasets) {
                if (ds.containerKey) ds.hidden = !isContainerSelected(ds.containerKey);
            }
            if (!state.loadingHistory) chart.update('none');
        }
        if (card) card.classList.toggle('hidden', !showCards);
    }

    const filterRoot = document.getElementById('container-app-filter');
    if (filterRoot) filterRoot.classList.toggle('hidden', !hasLive);

    if (hasLive) {
        updateSearchTerms();
        renderSharedLegend();
        updateFilterButtonLabel();
        renderFilterList();
    } else {
        document.getElementById('container-app-legend')?.classList.add('hidden');
    }

    // Focus mode moves container cards into the main grid; keep the Applications
    // header (title + filter + legend) with them, since the shared legend is the
    // only place series colors are labelled.
    syncApplicationsHeaderFocus();
}

/** Keep the Applications header visible while any container card is visible. */
export function syncApplicationsHeaderFocus() {
    pinSectionHeadForCards(document.getElementById('applications-header'), CONTAINER_CARD_SELECTOR);
}

/**
 * Expose container names to the chart search. Card titles are metric names now
 * ("CPU Usage"), so without this a search for a container name would hide every
 * container chart.
 */
function updateSearchTerms() {
    const apps = Object.values(state.containerApps || {}).filter(a => isContainerSelected(a.key));
    const terms = new Set();
    for (const app of apps) {
        terms.add(app.label);
        if (app.id) terms.add(app.id);
    }
    // Localized and ASCII generics so "container(s)" works in every locale.
    terms.add(i18n.t('containers'));
    terms.add(i18n.t('applications'));
    terms.add('container');
    terms.add('containers');
    const joined = [...terms].join(' ').toLowerCase();
    for (const m of CONTAINER_METRICS) {
        const card = document.getElementById(m.cardId);
        if (card) card.dataset.searchTerms = joined;
    }
}

function updateMetricSubtitles() {
    const allKeys = Object.keys(state.containerApps || {});
    const selectedKeys = allKeys.filter(k => isContainerSelected(k));
    const n = selectedKeys.length;
    const total = allKeys.length;
    const summary = n === total
        ? i18n.t('apps_count').replace('{n}', total)
        : i18n.t('apps_count_filtered').replace('{n}', n).replace('{total}', total);
    const latest = state._containerLatestByKey || {};
    const only = n === 1 ? latest[selectedKeys[0]] : null;

    for (const m of CONTAINER_METRICS) {
        const el = document.getElementById(m.subtitleId);
        if (!el) continue;
        if (only && m.key === 'mem') {
            const used = formatBytesShort(only.mem_used || 0);
            el.textContent = only.mem_limit > 0
                ? i18n.t('mem_used_limit')
                    .replace('{used}', used)
                    .replace('{limit}', formatBytesShort(only.mem_limit))
                    .replace('{pct}', (only.mem_pct || 0).toFixed(1))
                : i18n.t('mem_used_only').replace('{used}', used);
        } else if (only && m.key === 'cpu') {
            el.textContent = i18n.t('cpu_mem_summary')
                .replace('{cpu}', (only.cpu_pct || 0).toFixed(1))
                .replace('{mem}', formatBytesShort(only.mem_used || 0));
        } else {
            el.textContent = summary;
        }
    }
}

// ---- Filter UI ----

function ensureSharedLegendEl() {
    const header = document.getElementById('applications-header');
    if (!header) return null;
    let legend = document.getElementById('container-app-legend');
    if (legend) return legend;
    legend = document.createElement('div');
    legend.id = 'container-app-legend';
    legend.className = 'container-app-legend hidden';
    legend.setAttribute('aria-label', i18n.t('container_legend'));
    header.appendChild(legend);
    return legend;
}

function sortedApps(selectedOnly) {
    return Object.values(state.containerApps || {})
        .filter(a => !selectedOnly || isContainerSelected(a.key))
        .sort((a, b) => a.label.localeCompare(b.label, undefined, { sensitivity: 'base' }));
}

function renderSharedLegend() {
    const sig = `${appsSignature()}${selectionSignature()}`;
    if (renderState.selection === sig) return;
    renderState.selection = sig;

    const legend = ensureSharedLegendEl();
    if (!legend) return;
    legend.replaceChildren();

    const apps = sortedApps(true);
    if (apps.length === 0) {
        legend.classList.add('hidden');
        return;
    }
    legend.classList.remove('hidden');

    for (const app of apps) {
        const item = document.createElement('span');
        item.className = 'container-app-legend-item';
        item.title = app.label;

        const swatch = document.createElement('span');
        swatch.className = 'container-app-legend-swatch';
        swatch.style.backgroundColor = app.color;
        if (app.dash?.length) swatch.classList.add('is-dashed');

        const name = document.createElement('span');
        name.className = 'container-app-legend-name';
        name.textContent = app.label;

        item.appendChild(swatch);
        item.appendChild(name);
        legend.appendChild(item);
    }
    // Legend width changes can flip whether the header should stay pinned.
    syncApplicationsHeaderFocus();
}

function ensureFilterUI() {
    const header = document.getElementById('applications-header');
    if (!header) return null;

    // Prefer static structure from index.html (.section-head-row already present).
    let titleRow = header.querySelector('.section-head-row');
    if (!titleRow) {
        // Fallback for unexpected markup: wrap existing children once.
        titleRow = document.createElement('div');
        titleRow.className = 'section-head-row';
        while (header.firstChild) titleRow.appendChild(header.firstChild);
        header.appendChild(titleRow);
    }

    let root = document.getElementById('container-app-filter');
    if (root) {
        ensureSharedLegendEl();
        return root;
    }

    root = document.createElement('div');
    root.id = 'container-app-filter';
    root.className = 'container-app-filter hidden';

    const btn = document.createElement('button');
    btn.type = 'button';
    btn.id = 'container-app-filter-btn';
    btn.className = 'container-app-filter-btn';
    btn.setAttribute('aria-haspopup', 'true');
    btn.setAttribute('aria-expanded', 'false');
    const btnLabel = document.createElement('span');
    btnLabel.className = 'container-app-filter-btn-label';
    btnLabel.textContent = i18n.t('applications');
    const btnCount = document.createElement('span');
    btnCount.className = 'container-app-filter-btn-count';
    btnCount.id = 'container-app-filter-count';
    const caret = document.createElement('span');
    caret.className = 'container-app-filter-caret';
    caret.textContent = '▾';
    btn.append(btnLabel, btnCount, caret);

    const panel = document.createElement('div');
    panel.id = 'container-app-filter-panel';
    panel.className = 'container-app-filter-panel hidden';
    panel.setAttribute('role', 'menu');

    const actions = document.createElement('div');
    actions.className = 'container-app-filter-actions';
    const selAll = document.createElement('button');
    selAll.type = 'button';
    selAll.className = 'container-app-filter-action';
    selAll.dataset.action = 'select-all';
    selAll.textContent = i18n.t('select_all');
    const deselAll = document.createElement('button');
    deselAll.type = 'button';
    deselAll.className = 'container-app-filter-action';
    deselAll.dataset.action = 'deselect-all';
    deselAll.textContent = i18n.t('deselect_all');
    actions.append(selAll, deselAll);

    const list = document.createElement('div');
    list.id = 'container-app-filter-list';
    list.className = 'container-app-filter-list';

    panel.append(actions, list);
    root.append(btn, panel);
    titleRow.appendChild(root);
    ensureSharedLegendEl();

    btn.addEventListener('click', (e) => {
        e.stopPropagation();
        const open = panel.classList.toggle('hidden') === false;
        btn.setAttribute('aria-expanded', open ? 'true' : 'false');
    });

    panel.addEventListener('click', (e) => e.stopPropagation());

    document.addEventListener('click', () => {
        if (!panel.classList.contains('hidden')) {
            panel.classList.add('hidden');
            btn.setAttribute('aria-expanded', 'false');
        }
    });

    const applySelectionChange = () => {
        saveExcluded();
        refreshFilterCheckboxes();
        syncContainerMetricsUI(state._containerLiveKeys || new Set());
        updateMetricSubtitles();
    };

    selAll.addEventListener('click', (e) => {
        e.stopPropagation();
        selectAllContainers();
        applySelectionChange();
    });

    deselAll.addEventListener('click', (e) => {
        e.stopPropagation();
        deselectAllContainers();
        applySelectionChange();
    });

    // One delegated listener survives list rebuilds.
    list.addEventListener('change', (e) => {
        const chk = e.target;
        if (!(chk instanceof HTMLInputElement) || chk.type !== 'checkbox') return;
        setContainerSelected(chk.dataset.key, chk.checked);
        applySelectionChange();
    });

    return root;
}

/** Update checkbox state in place — never rebuild the list under the cursor. */
function refreshFilterCheckboxes() {
    const list = document.getElementById('container-app-filter-list');
    if (!list) return;
    for (const chk of list.querySelectorAll('input[type="checkbox"][data-key]')) {
        chk.checked = isContainerSelected(chk.dataset.key);
    }
}

/** Rebuild the filter rows. Membership-driven only, so open panels stay stable. */
function renderFilterList() {
    const list = document.getElementById('container-app-filter-list');
    if (!list) return;

    const sig = appsSignature();
    if (renderState.apps === sig) {
        refreshFilterCheckboxes();
        return;
    }
    renderState.apps = sig;
    list.replaceChildren();

    const apps = sortedApps(false);
    if (apps.length === 0) {
        const empty = document.createElement('div');
        empty.className = 'container-app-filter-empty';
        empty.textContent = i18n.t('no_containers');
        list.appendChild(empty);
        return;
    }

    for (const app of apps) {
        const row = document.createElement('label');
        row.className = 'container-app-filter-item';

        const chk = document.createElement('input');
        chk.type = 'checkbox';
        chk.checked = isContainerSelected(app.key);
        chk.dataset.key = app.key;

        const swatch = document.createElement('span');
        swatch.className = 'container-app-filter-swatch';
        swatch.style.backgroundColor = app.color;
        if (app.dash?.length) swatch.classList.add('is-dashed');

        const name = document.createElement('span');
        name.className = 'container-app-filter-name';
        name.textContent = app.label;
        name.title = app.label;

        row.append(chk, swatch, name);
        list.appendChild(row);
    }
}

function updateFilterButtonLabel() {
    const countEl = document.getElementById('container-app-filter-count');
    if (!countEl) return;
    const total = Object.keys(state.containerApps || {}).length;
    if (total === 0) {
        countEl.textContent = '';
        return;
    }
    const n = Object.keys(state.containerApps).filter(k => isContainerSelected(k)).length;
    countEl.textContent = n === total ? `(${total})` : `(${n}/${total})`;
}

/**
 * Process one sample's container list.
 * @param {object[]} containers
 * @param {number|Date} ts sample timestamp
 * @param {function} point (v) => ({x: ts, y: v})
 * @param {function} createAppChartCard
 * @returns {boolean} true if any containers were present
 */
export function addContainerSample(containers, ts, point, createAppChartCard) {
    ensureFilterState();
    ensureFilterUI();

    const list = containers || [];
    const tsMs = ts instanceof Date ? ts.getTime() : Number(ts);
    const liveKeys = new Set();
    const liveByKey = {};

    if (list.length === 0) {
        state._containerLiveKeys = liveKeys;
        state._containerLatestByKey = {};
        // Keep series time-aligned even while nothing is reporting.
        if (Object.keys(state.containerCharts || {}).length > 0) {
            appendAlignedTick(ts, liveByKey, point);
        }
        pruneDeadContainers(liveKeys, tsMs);
        syncContainerMetricsUI(liveKeys);
        updateMetricSubtitles();
        return false;
    }

    ensureMetricCharts(createAppChartCard);

    for (const ct of list) {
        const key = containerSeriesKey(ct);
        const app = ensureContainerApp(key, containerDisplayName(ct), ct.id, tsMs);
        ensureSeriesForApp(app, ts);
        liveKeys.add(key);
        liveByKey[key] = ct;
    }

    // One aligned tick for ALL known series (present → value, absent → null)
    appendAlignedTick(ts, liveByKey, point);
    pruneDeadContainers(liveKeys, tsMs);

    state._containerLiveKeys = liveKeys;
    state._containerLatestByKey = liveByKey;
    syncContainerMetricsUI(liveKeys);
    updateMetricSubtitles();
    return true;
}

/** Hide container metric UI when no containers in this sample (nginx may still show). */
export function markContainersAbsent(ts, point) {
    return addContainerSample([], ts, point, () => null);
}
