/* ============================================================
   container-apps.js — Multi-series container charts (one chart
   per metric type) + multi-select application filter.
   ============================================================ */
'use strict';
import { state, colors } from './state.js';
import { formatBytesShort } from './utils.js';
import { createTimeSeriesChart } from './charts-init.js';
import { attachDynamicChartCardActions } from './chart-card-actions.js';

// Palette for assigning a stable color per container/app.
const CONTAINER_COLOR_LIST = [
    colors.blue, colors.green, colors.orange, colors.purple, colors.cyan,
    colors.red, colors.yellow, colors.pink, colors.teal, colors.lime,
];

// Metric chart definitions: one chart per metric type, one series per app.
// I/O is split (Net Rx/Tx, Disk Read/Write) so each line keeps a unique app
// color without overlapping Rx/Tx or R/W on the same canvas.
const CONTAINER_METRICS = [
    {
        key: 'cpu',
        cardId: 'card-containers-cpu',
        chartId: 'chart-containers-cpu',
        subtitleId: 'containers-cpu-subtitle',
        title: 'Containers \u2014 CPU',
        order: 20,
        yConfig: { beginAtZero: true, ticks: { callback: v => v + '%' } },
        series: [{ field: 'cpu_pct', labelSuffix: '', dash: null }],
    },
    {
        key: 'mem',
        cardId: 'card-containers-mem',
        chartId: 'chart-containers-mem',
        subtitleId: 'containers-mem-subtitle',
        title: 'Containers \u2014 Memory',
        order: 21,
        yConfig: { beginAtZero: true, ticks: { callback: v => formatBytesShort(v) } },
        series: [{ field: 'mem_used', labelSuffix: '', dash: null }],
    },
    {
        key: 'net_rx',
        cardId: 'card-containers-net-rx',
        chartId: 'chart-containers-net-rx',
        subtitleId: 'containers-net-rx-subtitle',
        title: 'Containers \u2014 Network Rx',
        order: 22,
        yConfig: { beginAtZero: true, ticks: { callback: v => formatBytesShort(v) + '/s' } },
        series: [{ field: 'net_rx_bps', labelSuffix: '', dash: null }],
    },
    {
        key: 'net_tx',
        cardId: 'card-containers-net-tx',
        chartId: 'chart-containers-net-tx',
        subtitleId: 'containers-net-tx-subtitle',
        title: 'Containers \u2014 Network Tx',
        order: 23,
        yConfig: { beginAtZero: true, ticks: { callback: v => formatBytesShort(v) + '/s' } },
        series: [{ field: 'net_tx_bps', labelSuffix: '', dash: null }],
    },
    {
        key: 'disk_r',
        cardId: 'card-containers-disk-r',
        chartId: 'chart-containers-disk-r',
        subtitleId: 'containers-disk-r-subtitle',
        title: 'Containers \u2014 Disk Read',
        order: 24,
        yConfig: { beginAtZero: true, ticks: { callback: v => formatBytesShort(v) + '/s' } },
        series: [{ field: 'disk_r_bps', labelSuffix: '', dash: null }],
    },
    {
        key: 'disk_w',
        cardId: 'card-containers-disk-w',
        chartId: 'chart-containers-disk-w',
        subtitleId: 'containers-disk-w-subtitle',
        title: 'Containers \u2014 Disk Write',
        order: 25,
        yConfig: { beginAtZero: true, ticks: { callback: v => formatBytesShort(v) + '/s' } },
        series: [{ field: 'disk_w_bps', labelSuffix: '', dash: null }],
    },
];

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

function loadFilterSelection() {
    try {
        const raw = localStorage.getItem(FILTER_STORAGE_KEY);
        if (!raw) return null; // null = no saved preference → all selected
        const arr = JSON.parse(raw);
        if (Array.isArray(arr)) return new Set(arr);
    } catch (_) { /* ignore */ }
    return null;
}

function saveFilterSelection() {
    // Persist only when user has deselected something; null means all.
    const known = Object.keys(state.containerApps || {});
    if (known.length === 0) return;
    const selected = known.filter(k => isContainerSelected(k));
    if (selected.length === known.length) {
        localStorage.removeItem(FILTER_STORAGE_KEY);
    } else {
        localStorage.setItem(FILTER_STORAGE_KEY, JSON.stringify(selected));
    }
}

export function isContainerSelected(key) {
    // Missing entry in selected set map: treat as selected (new apps default on).
    if (!state.containerFilter) return true;
    // containerFilter is a Set of selected keys; if the key is unknown to the
    // set because it is brand new, default to selected.
    if (!state.containerApps[key]) return true;
    return state.containerFilter.has(key);
}

function ensureFilterState() {
    if (!state.containerFilter) {
        const saved = loadFilterSelection();
        state.containerFilter = saved; // may be null → all selected
    }
}

function ensureContainerApp(key, label) {
    if (!state.containerApps) state.containerApps = {};
    if (state.containerApps[key]) {
        state.containerApps[key].label = label;
        return state.containerApps[key];
    }
    // Prefer an unused palette slot; wrap when there are more apps than colors.
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
    ensureFilterState();
    if (state.containerFilter) {
        // New app: selected by default
        state.containerFilter.add(key);
    }
    return state.containerApps[key];
}

function ensureMetricCharts(createAppChartCard, resetZoomAll) {
    if (!state.containerCharts) state.containerCharts = {};
    for (const m of CONTAINER_METRICS) {
        if (state.containerCharts[m.key]) continue;
        createAppChartCard(m.cardId, m.chartId, m.subtitleId, m.title, m.order);
        // Empty datasets — series are added per container dynamically.
        // Per-chart legends are disabled: a single shared legend lives under
        // the Applications section title (see renderSharedLegend).
        const chart = createTimeSeriesChart(m.chartId, [], m.yConfig);
        if (chart?.options?.plugins?.legend) {
            chart.options.plugins.legend.display = false;
        }
        state.containerCharts[m.key] = chart;
        // Tag card for filter visibility logic
        const card = document.getElementById(m.cardId);
        if (card) card.dataset.containerMetric = m.key;
    }
}

function ensureSeriesForApp(app) {
    for (const m of CONTAINER_METRICS) {
        const chart = state.containerCharts[m.key];
        if (!chart) continue;
        for (const s of m.series) {
            const dsId = app.key + '::' + s.field;
            let ds = chart.data.datasets.find(d => d._dsId === dsId);
            if (ds) {
                ds.label = app.label + s.labelSuffix;
                ds.borderColor = app.color;
                ds.backgroundColor = hexToRgba(app.color, 0.12);
                ds.hidden = !isContainerSelected(app.key);
                continue;
            }
            ds = {
                _dsId: dsId,
                containerKey: app.key,
                label: app.label + s.labelSuffix,
                borderColor: app.color,
                backgroundColor: hexToRgba(app.color, 0.12),
                fill: m.series.length === 1,
                data: [],
                pointRadius: 0,
                borderWidth: 1.5,
                tension: 0.2,
                borderDash: s.dash || [],
                hidden: !isContainerSelected(app.key),
            };
            chart.data.datasets.push(ds);
        }
    }
}

function applyFilterToCharts() {
    for (const m of CONTAINER_METRICS) {
        const chart = state.containerCharts?.[m.key];
        if (!chart) continue;
        let anyVisible = false;
        for (const ds of chart.data.datasets) {
            const selected = isContainerSelected(ds.containerKey);
            ds.hidden = !selected;
            if (selected) anyVisible = true;
        }
        chart.update('none');
        // Hide chart card entirely if nothing selected
        const card = document.getElementById(m.cardId);
        if (card) card.classList.toggle('hidden', !anyVisible && chart.data.datasets.length > 0);
    }
    updateMetricSubtitles();
    renderSharedLegend();
}

function updateMetricSubtitles() {
    const selected = Object.values(state.containerApps || {}).filter(a => isContainerSelected(a.key));
    const n = selected.length;
    const total = Object.keys(state.containerApps || {}).length;
    const summary = n === total ? `${total} apps` : `${n}/${total} apps`;

    for (const m of CONTAINER_METRICS) {
        const el = document.getElementById(m.subtitleId);
        if (el) el.textContent = summary;
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
    legend.className = 'container-app-legend';
    legend.setAttribute('aria-label', 'Container color legend');
    header.appendChild(legend);
    return legend;
}

// Single shared color legend under Applications (not repeated on each chart).
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
    if (!header) return;
    let root = document.getElementById('container-app-filter');
    if (root) {
        ensureSharedLegendEl();
        return root;
    }

    // Title row: keep filter button aligned with the section title.
    let titleRow = header.querySelector('.section-head-row');
    if (!titleRow) {
        titleRow = document.createElement('div');
        titleRow.className = 'section-head-row';
        // Move existing title (and anything else already in the header) into the row
        while (header.firstChild) titleRow.appendChild(header.firstChild);
        header.appendChild(titleRow);
    }

    root = document.createElement('div');
    root.id = 'container-app-filter';
    root.className = 'container-app-filter';

    const btn = document.createElement('button');
    btn.type = 'button';
    btn.id = 'container-app-filter-btn';
    btn.className = 'container-app-filter-btn';
    btn.setAttribute('aria-haspopup', 'true');
    btn.setAttribute('aria-expanded', 'false');
    btn.innerHTML = '<span class="container-app-filter-btn-label">Applications</span><span class="container-app-filter-btn-count" id="container-app-filter-count"></span><span class="container-app-filter-caret">▾</span>';

    const panel = document.createElement('div');
    panel.id = 'container-app-filter-panel';
    panel.className = 'container-app-filter-panel hidden';
    panel.setAttribute('role', 'menu');

    const actions = document.createElement('div');
    actions.className = 'container-app-filter-actions';
    const selAll = document.createElement('button');
    selAll.type = 'button';
    selAll.className = 'container-app-filter-action';
    selAll.textContent = 'Select all';
    const deselAll = document.createElement('button');
    deselAll.type = 'button';
    deselAll.className = 'container-app-filter-action';
    deselAll.textContent = 'Deselect all';
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
        ensureFilterState();
        if (!state.containerFilter) state.containerFilter = new Set();
        Object.keys(state.containerApps || {}).forEach(k => state.containerFilter.add(k));
        // All selected → store as null semantics
        state.containerFilter = new Set(Object.keys(state.containerApps || {}));
        saveFilterSelection();
        renderFilterList();
        applyFilterToCharts();
        updateFilterButtonLabel();
    });

    deselAll.addEventListener('click', (e) => {
        e.stopPropagation();
        ensureFilterState();
        state.containerFilter = new Set(); // empty = none selected
        saveFilterSelection();
        renderFilterList();
        applyFilterToCharts();
        updateFilterButtonLabel();
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
            ensureFilterState();
            if (!state.containerFilter) {
                // Was "all selected" (null). Materialize full set then toggle.
                state.containerFilter = new Set(Object.keys(state.containerApps || {}));
            }
            if (chk.checked) state.containerFilter.add(app.key);
            else state.containerFilter.delete(app.key);
            saveFilterSelection();
            applyFilterToCharts();
            updateFilterButtonLabel();
        });

        row.appendChild(chk);
        row.appendChild(swatch);
        row.appendChild(name);
        list.appendChild(row);
    }
}

function updateFilterButtonLabel() {
    const countEl = document.getElementById('container-app-filter-count');
    const filterRoot = document.getElementById('container-app-filter');
    const total = Object.keys(state.containerApps || {}).length;
    if (!countEl || !filterRoot) return;
    if (total === 0) {
        filterRoot.classList.add('hidden');
        return;
    }
    filterRoot.classList.remove('hidden');
    const n = Object.keys(state.containerApps || {}).filter(k => isContainerSelected(k)).length;
    countEl.textContent = n === total ? `(${total})` : `(${n}/${total})`;
}

/**
 * Process one sample's container list: ensure charts/series exist, push points.
 * @param {object[]} containers - sample.apps.containers
 * @param {{x:number,y:number}} point - helper that wraps a value
 * @param {function} createAppChartCard
 * @param {function} resetZoomAll
 * @returns {boolean} true if any containers were present
 */
export function addContainerSample(containers, point, createAppChartCard, resetZoomAll) {
    if (!containers || containers.length === 0) {
        // Hide metric cards if we ever had them and no containers remain
        return false;
    }

    ensureFilterUI();
    ensureMetricCharts(createAppChartCard, resetZoomAll);

    const seen = new Set();
    const latestByKey = {};

    for (const ct of containers) {
        const key = containerSeriesKey(ct);
        const label = containerDisplayName(ct);
        const app = ensureContainerApp(key, label);
        ensureSeriesForApp(app);
        seen.add(key);
        latestByKey[key] = ct;

        for (const m of CONTAINER_METRICS) {
            const chart = state.containerCharts[m.key];
            if (!chart) continue;
            for (const s of m.series) {
                const dsId = key + '::' + s.field;
                const ds = chart.data.datasets.find(d => d._dsId === dsId);
                if (!ds) continue;
                const val = ct[s.field] || 0;
                ds.data.push(point(val));
                ds.hidden = !isContainerSelected(key);
            }
            if (!state.loadingHistory) chart.update('none');
        }
    }

    // Hide series for containers not in this sample (stale)
    for (const m of CONTAINER_METRICS) {
        const chart = state.containerCharts[m.key];
        if (!chart) continue;
        for (const ds of chart.data.datasets) {
            if (ds.containerKey && !seen.has(ds.containerKey)) {
                // Keep historical data; just leave as-is. Optionally mark inactive.
            }
        }
        // Show cards
        document.getElementById(m.cardId)?.classList.remove('hidden');
    }

    // Richer memory subtitle when a single app is selected
    const selectedKeys = Object.keys(state.containerApps || {}).filter(k => isContainerSelected(k));
    const memSub = document.getElementById('containers-mem-subtitle');
    if (memSub && selectedKeys.length === 1 && latestByKey[selectedKeys[0]]) {
        const ct = latestByKey[selectedKeys[0]];
        const used = formatBytesShort(ct.mem_used || 0);
        if (ct.mem_limit > 0) {
            memSub.textContent = `Used: ${used} / Limit: ${formatBytesShort(ct.mem_limit)} (${(ct.mem_pct || 0).toFixed(1)}%)`;
        } else {
            memSub.textContent = `Used: ${used}`;
        }
    } else {
        updateMetricSubtitles();
    }

    const cpuSub = document.getElementById('containers-cpu-subtitle');
    if (cpuSub && selectedKeys.length === 1 && latestByKey[selectedKeys[0]]) {
        const ct = latestByKey[selectedKeys[0]];
        cpuSub.textContent = `CPU: ${(ct.cpu_pct || 0).toFixed(1)}%  Mem: ${formatBytesShort(ct.mem_used || 0)}`;
    }

    updateFilterButtonLabel();
    renderSharedLegend();
    // Keep list fresh if panel open
    const panel = document.getElementById('container-app-filter-panel');
    if (panel && !panel.classList.contains('hidden')) renderFilterList();

    return true;
}

export function destroyContainerMetricCharts() {
    for (const m of CONTAINER_METRICS) {
        const chart = state.containerCharts?.[m.key];
        if (chart) chart.destroy();
        document.getElementById(m.cardId)?.remove();
    }
    // Also remove any legacy combined I/O cards from earlier iterations
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
    const filter = document.getElementById('container-app-filter');
    if (filter) filter.classList.toggle('hidden', !show);
    const legend = document.getElementById('container-app-legend');
    if (legend && !show) legend.classList.add('hidden');
}
