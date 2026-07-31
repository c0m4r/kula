/* ============================================================
   container-apps.js — Multi-series container charts (one chart
   per metric type) + multi-select application filter.

   Review fixes (PR #42):
   - Filter persists as exclusions so reload keeps deselections
   - Every sample tick appends aligned points (null backfill)
   - Card visibility is reconciled via syncContainerMetricsUI
   ============================================================ */
'use strict';
import { state, colors } from './state.js';
import { formatBytesShort } from './utils.js';
import { createTimeSeriesChart } from './charts-init.js';
import { i18n } from './i18n.js';

// Palette for assigning a stable color per container/app.
const CONTAINER_COLOR_LIST = [
    colors.blue, colors.green, colors.orange, colors.purple, colors.cyan,
    colors.red, colors.yellow, colors.pink, colors.teal, colors.lime,
];

// Metric chart definitions: one chart per metric type, one series per app.
// Titles reuse the same i18n keys as System Metrics wherever possible
// (cpu_usage, memory_usage, network_throughput, disk_io, rx/tx, read_bs/write_bs).
const CONTAINER_METRICS = [
    {
        key: 'cpu',
        cardId: 'card-containers-cpu',
        chartId: 'chart-containers-cpu',
        subtitleId: 'containers-cpu-subtitle',
        // Same label as System Metrics → "CPU Usage" / "Usage CPU" / …
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
        // System "Disk I/O" chart series use read_bs / write_bs.
        i18nKeys: ['disk_io', 'read_bs'],
        order: 24,
        yConfig: { beginAtZero: true, ticks: { callback: v => formatBytesShort(v) + '/s' } },
        field: 'disk_r_bps',
    },
    {
        key: 'disk_w',
        cardId: 'card-containers-disk-w',
        chartId: 'chart-containers-disk-w',
        subtitleId: 'containers-disk-w-subtitle',
        i18nKeys: ['disk_io', 'write_bs'],
        order: 25,
        yConfig: { beginAtZero: true, ticks: { callback: v => formatBytesShort(v) + '/s' } },
        field: 'disk_w_bps',
    },
];

/** Build a chart title from one or more existing i18n keys (no new locale strings). */
function metricTitle(m) {
    const keys = m.i18nKeys || [];
    if (keys.length === 0) return m.key;
    if (keys.length === 1) return i18n.t(keys[0]);
    // e.g. "Network Throughput (RX)" / "Netzwerkdurchsatz (RX)"
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
    // Filter chrome
    const btnLabel = document.querySelector('.container-app-filter-btn-label');
    if (btnLabel) btnLabel.textContent = i18n.t('applications');
    const selAll = document.querySelector('#container-app-filter-panel .container-app-filter-action:first-child');
    const deselAll = document.querySelector('#container-app-filter-panel .container-app-filter-action:last-child');
    // Prefer dedicated keys when present; fall back to English literals.
    if (selAll) selAll.textContent = i18n.t('select_all') !== 'select_all' ? i18n.t('select_all') : 'Select all';
    if (deselAll) deselAll.textContent = i18n.t('deselect_all') !== 'deselect_all' ? i18n.t('deselect_all') : 'Deselect all';
    updateFilterButtonLabel();
    renderSharedLegend();
    const panel = document.getElementById('container-app-filter-panel');
    if (panel && !panel.classList.contains('hidden')) renderFilterList();
}

// Re-apply titles when the user switches language.
if (typeof document !== 'undefined') {
    document.addEventListener('kula-i18n-changed', retranslateContainerUI);
}

const FILTER_STORAGE_KEY = 'kula_container_filter';

// Stable chart/app key for a container sample. Prefer the human-readable name so
// history survives recreate (Docker assigns a new ID each time). Fall back to
// id when name is empty (cgroups-only discovery has no names).
export function containerSeriesKey(ct) {
    const raw = (ct.name && String(ct.name).trim()) || ct.id || 'unknown';
    return 'container_' + encodeURIComponent(String(raw));
}

export function containerDisplayName(ct) {
    return (ct.name && String(ct.name).trim()) || ct.id || 'unknown';
}

function hexToRgba(hex, alpha) {
    const h = hex.replace('#', '');
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
        // New format
        if (parsed && Array.isArray(parsed.excluded)) {
            return new Set(parsed.excluded);
        }
        // Legacy format was a selected-keys array — migrate best-effort:
        // we cannot know full universe at load time, so drop legacy and start fresh
        // (safer than re-selecting everything after user deselected all).
        if (Array.isArray(parsed)) {
            // Treat legacy empty selection as "exclude all known later" is impossible
            // without known keys; store as empty exclusions and let user re-filter.
            // Prefer: if legacy was empty array, mark a sentinel so first discover
            // deselects all? Too magic. Clear legacy key.
            localStorage.removeItem(FILTER_STORAGE_KEY);
            return new Set();
        }
    } catch (_) { /* ignore */ }
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

export function isContainerSelected(key) {
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

function ensureContainerApp(key, label) {
    if (!state.containerApps) state.containerApps = {};
    if (state.containerApps[key]) {
        state.containerApps[key].label = label;
        return state.containerApps[key];
    }
    const used = new Set(Object.values(state.containerApps).map(a => a.colorIndex));
    let colorIndex = 0;
    for (let i = 0; i < CONTAINER_COLOR_LIST.length; i++) {
        if (!used.has(i)) { colorIndex = i; break; }
        colorIndex = i % CONTAINER_COLOR_LIST.length;
    }
    if (used.size >= CONTAINER_COLOR_LIST.length) {
        colorIndex = Object.keys(state.containerApps).length % CONTAINER_COLOR_LIST.length;
    }
    const color = CONTAINER_COLOR_LIST[colorIndex];
    state.containerApps[key] = { key, label, color, colorIndex };
    // New apps are selected by default: do NOT add to containerExcluded.
    ensureFilterState();
    return state.containerApps[key];
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
        let ds = chart.data.datasets.find(d => d._dsId === app.key);
        if (ds) {
            ds.label = app.label;
            ds.borderColor = app.color;
            ds.backgroundColor = hexToRgba(app.color, 0.12);
            ds.hidden = !isContainerSelected(app.key);
            continue;
        }
        ds = {
            _dsId: app.key,
            containerKey: app.key,
            label: app.label,
            borderColor: app.color,
            backgroundColor: hexToRgba(app.color, 0.12),
            fill: false,
            data: alignedNullPrefix(chart, ts),
            pointRadius: 0,
            borderWidth: 1.5,
            tension: 0.2,
            hidden: !isContainerSelected(app.key),
        };
        chart.data.datasets.push(ds);
    }
}

/**
 * Append one tick to every known series on every metric chart.
 * Present containers get real values; others get {x:ts, y:null} so dataset
 * lengths stay aligned for Chart.js interaction.mode: 'index'.
 */
function appendAlignedTick(ts, liveByKey, point) {
    for (const m of CONTAINER_METRICS) {
        const chart = state.containerCharts?.[m.key];
        if (!chart) continue;
        for (const ds of chart.data.datasets) {
            const key = ds.containerKey;
            if (!key) continue;
            const ct = liveByKey[key];
            if (ct) {
                ds.data.push(point(ct[m.field] || 0));
            } else {
                ds.data.push({ x: ts, y: null });
            }
            ds.hidden = !isContainerSelected(key);
        }
        if (!state.loadingHistory) chart.update('none');
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
        renderSharedLegend();
        updateFilterButtonLabel();
        // Subtitles with live values are set by addContainerSample(liveByKey).
        // Here only refresh the generic "N apps" summary when we lack latests.
        if (!(state._containerLatestByKey)) updateMetricSubtitles({});
    } else {
        const legend = document.getElementById('container-app-legend');
        if (legend) legend.classList.add('hidden');
    }

    const panel = document.getElementById('container-app-filter-panel');
    if (panel && !panel.classList.contains('hidden')) renderFilterList();
}

function updateMetricSubtitles(latestByKey) {
    const selectedKeys = Object.keys(state.containerApps || {}).filter(k => isContainerSelected(k));
    const n = selectedKeys.length;
    const total = Object.keys(state.containerApps || {}).length;
    const summary = n === total ? `${total} apps` : `${n}/${total} apps`;
    const latest = latestByKey && typeof latestByKey === 'object' && !(latestByKey instanceof Set)
        ? latestByKey
        : (state._containerLatestByKey || {});

    for (const m of CONTAINER_METRICS) {
        const el = document.getElementById(m.subtitleId);
        if (!el) continue;
        if (m.key === 'mem' && selectedKeys.length === 1 && latest[selectedKeys[0]]) {
            const ct = latest[selectedKeys[0]];
            const used = formatBytesShort(ct.mem_used || 0);
            if (ct.mem_limit > 0) {
                el.textContent = `Used: ${used} / Limit: ${formatBytesShort(ct.mem_limit)} (${(ct.mem_pct || 0).toFixed(1)}%)`;
            } else {
                el.textContent = `Used: ${used}`;
            }
        } else if (m.key === 'cpu' && selectedKeys.length === 1 && latest[selectedKeys[0]]) {
            const ct = latest[selectedKeys[0]];
            el.textContent = `CPU: ${(ct.cpu_pct || 0).toFixed(1)}%  Mem: ${formatBytesShort(ct.mem_used || 0)}`;
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
    legend.setAttribute('aria-label', 'Container color legend');
    header.appendChild(legend);
    return legend;
}

function renderSharedLegend() {
    const legend = ensureSharedLegendEl();
    if (!legend) return;
    legend.replaceChildren();

    const apps = Object.values(state.containerApps || {})
        .filter(a => isContainerSelected(a.key))
        .sort((a, b) => a.label.localeCompare(b.label, undefined, { sensitivity: 'base' }));

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
        swatch.style.background = app.color;

        const name = document.createElement('span');
        name.className = 'container-app-legend-name';
        name.textContent = app.label;

        item.appendChild(swatch);
        item.appendChild(name);
        legend.appendChild(item);
    }
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
    btn.innerHTML = `<span class="container-app-filter-btn-label">${i18n.t('applications')}</span><span class="container-app-filter-btn-count" id="container-app-filter-count"></span><span class="container-app-filter-caret">▾</span>`;

    const panel = document.createElement('div');
    panel.id = 'container-app-filter-panel';
    panel.className = 'container-app-filter-panel hidden';
    panel.setAttribute('role', 'menu');

    const actions = document.createElement('div');
    actions.className = 'container-app-filter-actions';
    const selAll = document.createElement('button');
    selAll.type = 'button';
    selAll.className = 'container-app-filter-action';
    selAll.textContent = i18n.t('select_all') !== 'select_all' ? i18n.t('select_all') : 'Select all';
    const deselAll = document.createElement('button');
    deselAll.type = 'button';
    deselAll.className = 'container-app-filter-action';
    deselAll.textContent = i18n.t('deselect_all') !== 'deselect_all' ? i18n.t('deselect_all') : 'Deselect all';
    actions.appendChild(selAll);
    actions.appendChild(deselAll);

    const list = document.createElement('div');
    list.id = 'container-app-filter-list';
    list.className = 'container-app-filter-list';

    panel.appendChild(actions);
    panel.appendChild(list);
    root.appendChild(btn);
    root.appendChild(panel);
    titleRow.appendChild(root);
    ensureSharedLegendEl();

    btn.addEventListener('click', (e) => {
        e.stopPropagation();
        const open = panel.classList.toggle('hidden') === false;
        btn.setAttribute('aria-expanded', open ? 'true' : 'false');
        if (open) renderFilterList();
    });

    panel.addEventListener('click', (e) => e.stopPropagation());

    document.addEventListener('click', () => {
        if (!panel.classList.contains('hidden')) {
            panel.classList.add('hidden');
            btn.setAttribute('aria-expanded', 'false');
        }
    });

    selAll.addEventListener('click', (e) => {
        e.stopPropagation();
        selectAllContainers();
        renderFilterList();
        const live = state._containerLiveKeys || new Set(Object.keys(state.containerApps || {}));
        syncContainerMetricsUI(live);
        updateMetricSubtitles(state._containerLatestByKey || {});
    });

    deselAll.addEventListener('click', (e) => {
        e.stopPropagation();
        deselectAllContainers();
        renderFilterList();
        // Keep last live keys so we know containers still exist; cards hide via anySelected=false
        syncContainerMetricsUI(state._containerLiveKeys || new Set());
        updateMetricSubtitles(state._containerLatestByKey || {});
    });

    return root;
}

function renderFilterList() {
    const list = document.getElementById('container-app-filter-list');
    if (!list) return;
    list.replaceChildren();

    const apps = Object.values(state.containerApps || {}).sort((a, b) =>
        a.label.localeCompare(b.label, undefined, { sensitivity: 'base' })
    );

    if (apps.length === 0) {
        const empty = document.createElement('div');
        empty.className = 'container-app-filter-empty';
        empty.textContent = 'No containers yet';
        list.appendChild(empty);
        return;
    }

    for (const app of apps) {
        const row = document.createElement('label');
        row.className = 'container-app-filter-item';

        const swatch = document.createElement('span');
        swatch.className = 'container-app-filter-swatch';
        swatch.style.background = app.color;
        swatch.title = app.color;

        const chk = document.createElement('input');
        chk.type = 'checkbox';
        chk.checked = isContainerSelected(app.key);
        chk.dataset.key = app.key;

        const name = document.createElement('span');
        name.className = 'container-app-filter-name';
        name.textContent = app.label;

        chk.addEventListener('change', () => {
            setContainerSelected(app.key, chk.checked);
            saveExcluded();
            const live = state._containerLiveKeys || new Set(Object.keys(state.containerApps || {}));
            syncContainerMetricsUI(live);
            updateMetricSubtitles(state._containerLatestByKey || {});
        });

        row.appendChild(chk);
        row.appendChild(swatch);
        row.appendChild(name);
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
    const n = Object.keys(state.containerApps || {}).filter(k => isContainerSelected(k)).length;
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
    const liveKeys = new Set();
    const liveByKey = {};

    if (list.length === 0) {
        state._containerLiveKeys = liveKeys;
        // Still append null ticks so series stay aligned if charts already exist
        if (Object.keys(state.containerCharts || {}).length > 0) {
            appendAlignedTick(ts, liveByKey, point);
        }
        syncContainerMetricsUI(liveKeys);
        return false;
    }

    ensureMetricCharts(createAppChartCard);

    for (const ct of list) {
        const key = containerSeriesKey(ct);
        const label = containerDisplayName(ct);
        const app = ensureContainerApp(key, label);
        ensureSeriesForApp(app, ts);
        liveKeys.add(key);
        liveByKey[key] = ct;
    }

    // One aligned tick for ALL known series (present → value, absent → null)
    appendAlignedTick(ts, liveByKey, point);

    state._containerLiveKeys = liveKeys;
    state._containerLatestByKey = liveByKey;
    syncContainerMetricsUI(liveKeys);
    updateMetricSubtitles(liveByKey);
    return true;
}

/** Hide container metric UI when no containers in this sample (nginx may still show). */
export function markContainersAbsent(ts, point) {
    return addContainerSample([], ts, point, () => null);
}

export function destroyContainerMetricCharts() {
    for (const m of CONTAINER_METRICS) {
        const chart = state.containerCharts?.[m.key];
        if (chart) chart.destroy();
        document.getElementById(m.cardId)?.remove();
    }
    ['card-containers-net', 'card-containers-diskio'].forEach(id => {
        document.getElementById(id)?.remove();
    });
    if (state.containerCharts) {
        for (const m of CONTAINER_METRICS) delete state.containerCharts[m.key];
        delete state.containerCharts.net;
        delete state.containerCharts.diskio;
    }
}

export function showContainerFilter(show) {
    // Compatibility shim: visibility is owned by syncContainerMetricsUI.
    const filter = document.getElementById('container-app-filter');
    if (filter && !show) filter.classList.add('hidden');
    const legend = document.getElementById('container-app-legend');
    if (legend && !show) legend.classList.add('hidden');
}
