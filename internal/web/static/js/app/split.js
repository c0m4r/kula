/* ============================================================
   split.js — Per-device/interface graph splitting.
   Allows splitting multi-device charts into individual charts,
   configurable from the dashboard or config file.
   ============================================================ */
'use strict';
import { state, colors, getChartMaxBound } from './state.js';
import { diskKey, diskMember, diskLabel, diskTitle, diskDOMKey } from './disk-identity.js';
import { createTimeSeriesChart } from './charts-init.js';
import { formatBytesShort, formatMetricNumber, formatPPS } from './format.js';
import { i18n } from './i18n.js';
import { queueChartUpdate } from './chart-controller.js';
import { appendEnvelopeGap, appendEnvelopePoint, clearEnvelopeData, ensureSensorDatasets, hasEnvelopeData } from './chart-envelope.js';
import { setChartHidden } from './chart-ui.js';

let _redrawFromBuffer = null;
let _rebuilding = false;

// Cards that receive a split toggle button (string or array of card IDs)
const SPLIT_BTN_CARD = {
    network:   'card-network',
    diskio:    'card-disk-io',
    diskspace: 'card-disk-space',
    disktemp:  'card-disk-temp',
    gpu:       ['card-gpu-load', 'card-vram'],
};

// Cards to hide when split is active for a type
const SPLIT_ORIGINAL_CARDS = {
    network:   ['card-network', 'card-pps'],
    diskio:    ['card-disk-io'],
    diskspace: ['card-disk-space'],
    disktemp:  ['card-disk-temp'],
    gpu:       ['card-gpu-load', 'card-vram', 'card-gpu-temp'],
};

// state key and localStorage key per type
const SPLIT_STATE_KEY = {
    network:   'splitNet',
    diskio:    'splitDiskIo',
    diskspace: 'splitDiskSpace',
    disktemp:  'splitDiskTemp',
    gpu:       'splitGpu',
};

const SPLIT_LS_KEY = {
    network:   'kula_split_net',
    diskio:    'kula_split_diskio',
    diskspace: 'kula_split_diskspace',
    disktemp:  'kula_split_disktemp',
    gpu:       'kula_split_gpu',
};

// Cached option lists to detect changes
const splitOptionsCache = {};

export function initSplitModule(redrawFromBufferFn) {
    _redrawFromBuffer = redrawFromBufferFn;
    _addSplitButtons();
}

export function applySplitFromConfig(splitCfg) {
    if (!splitCfg) return;
    const keyMap = {
        network:   'network',
        disk_io:   'diskio',
        disk_space: 'diskspace',
        disk_temp: 'disktemp',
        gpu:       'gpu',
    };
    for (const [cfgKey, type] of Object.entries(keyMap)) {
        if (splitCfg[cfgKey] === true && !getSplitState(type)) {
            setSplitState(type, true);
            _updateSplitBtn(type);
        }
    }
}

// Called from updateSelectors in charts-data.js whenever device options may change
export function updateSplitSelectors(s) {
    if (_rebuilding) return;
    for (const type of Object.keys(SPLIT_BTN_CARD)) {
        if (!getSplitState(type)) continue;
        const options = _getOptions(type, s);
        if (options.length === 0) continue;
        const optKey = options.join(',');
        if (splitOptionsCache[type] !== optKey) {
            const prevOptions = splitOptionsCache[type] ? splitOptionsCache[type].split(',') : [];
            const newOptions = options.filter(o => !prevOptions.includes(o));
            splitOptionsCache[type] = optKey;
            _hideOriginalCards(type);
            _buildSplitChartsForType(type, options);
            _triggerRedraw();
            // Newly detected devices must start with empty metrics — clear any
            // data that the buffer replay may have populated for them.
            if (newOptions.length > 0 && prevOptions.length > 0) {
                _clearDataForOptions(type, newOptions);
            }
        }
        if (type === 'diskio' || type === 'disktemp') {
            // A rename changes display metadata without replacing the series.
            for (const key of options) {
                const title = document.getElementById(`card-split-${type}-${diskDOMKey(key)}`)?.querySelector('h3');
                const disk = state.diskDevices.get(key);
                if (title && disk) {
                    title.textContent = `${i18n.t(type === 'diskio' ? 'disk_io' : 'disk_temp')}: ${diskLabel(disk)}`;
                    title.title = diskTitle(disk);
                }
            }
        }
    }
}

// Called from addSampleToCharts in charts-data.js for every data point
export function addSampleToSplitCharts(s, minimum, maximum, ts, hasEnvelope = false) {
    const touchedDatasets = new Set();
    const push = (dataset, value, minValue, maxValue, extra = null) => {
        if (dataset) touchedDatasets.add(dataset);
        appendEnvelopePoint(
            dataset,
            ts,
            value,
            hasEnvelope ? minValue : null,
            hasEnvelope ? maxValue : null,
            extra,
        );
    };

    // Network
    if (state.splitNet && s.net?.ifaces && state.splitCharts.network) {
        for (const iface of s.net.ifaces) {
            if (iface.name === 'lo') continue;
            const minIface = _memberBy(minimum?.net?.ifaces, 'name', iface.name);
            const maxIface = _memberBy(maximum?.net?.ifaces, 'name', iface.name);
            const netChart = state.splitCharts.network[`net_${iface.name}`];
            const ppsChart = state.splitCharts.network[`pps_${iface.name}`];
            if (netChart?.data?.datasets) {
                push(netChart.data.datasets[0], iface.rx_mbps || 0, minIface?.rx_mbps, maxIface?.rx_mbps);
                push(netChart.data.datasets[1], iface.tx_mbps || 0, minIface?.tx_mbps, maxIface?.tx_mbps);
            }
            if (ppsChart?.data?.datasets) {
                push(ppsChart.data.datasets[0], iface.rx_pps || 0, minIface?.rx_pps, maxIface?.rx_pps);
                push(ppsChart.data.datasets[1], iface.tx_pps || 0, minIface?.tx_pps, maxIface?.tx_pps);
            }
        }
    }

    // Disk I/O
    if (state.splitDiskIo && s.disk?.devices && state.splitCharts.diskio) {
        for (const dev of s.disk.devices) {
            const minDev = diskMember(minimum?.disk?.devices, diskKey(dev));
            const maxDev = diskMember(maximum?.disk?.devices, diskKey(dev));
            const chart = state.splitCharts.diskio[`diskio_${diskKey(dev)}`];
            if (chart?.data?.datasets) {
                push(chart.data.datasets[0], dev.read_bps || 0, minDev?.read_bps, maxDev?.read_bps);
                push(chart.data.datasets[1], dev.write_bps || 0, minDev?.write_bps, maxDev?.write_bps);
                push(chart.data.datasets[2], dev.reads_ps || 0, minDev?.reads_ps, maxDev?.reads_ps);
                push(chart.data.datasets[3], dev.writes_ps || 0, minDev?.writes_ps, maxDev?.writes_ps);
            }
        }
    }

    // Disk Space
    if (state.splitDiskSpace && s.disk?.filesystems && state.splitCharts.diskspace) {
        for (const fs of s.disk.filesystems) {
            const minFS = _memberBy(minimum?.disk?.filesystems, 'mount', fs.mount);
            const maxFS = _memberBy(maximum?.disk?.filesystems, 'mount', fs.mount);
            const chart = state.splitCharts.diskspace[`diskspace_${fs.mount}`];
            if (chart?.data?.datasets) {
                push(
                    chart.data.datasets[0],
                    fs.used_pct || 0,
                    minFS?.used_pct,
                    maxFS?.used_pct,
                    { used: fs.used || 0, total: fs.total || 0 },
                );
            }
        }
    }

    // Disk Temp
    if (state.splitDiskTemp && state.splitCharts.disktemp) {
        const pairs = [
            [colors.red, colors.redAlpha],
            [colors.orange, colors.orangeAlpha],
            [colors.yellow, colors.yellowAlpha],
            [colors.pink, colors.pinkAlpha],
            [colors.purple, colors.purpleAlpha],
            [colors.cyan, colors.cyanAlpha],
        ];
        for (const dev of s.disk?.devices || []) {
            const chartKey = `disktemp_${diskKey(dev)}`;
            const chart = state.splitCharts.disktemp[chartKey];
            if (!chart) continue;
            const minDev = diskMember(minimum?.disk?.devices, diskKey(dev));
            const maxDev = diskMember(maximum?.disk?.devices, diskKey(dev));
            const hasSensors = Array.isArray(dev.sensors) && dev.sensors.length > 0;
            const hasTemp    = dev.temp > 0;

            if (hasSensors || hasTemp) {
                setChartHidden(`card-split-disktemp-${diskDOMKey(diskKey(dev))}`, false);
                setChartHidden('thermals-title', false);
                setChartHidden('thermals-grid', false);
            }

            const readings = hasSensors
                ? dev.sensors.map(sensor => ({
                    name: sensor.name,
                    value: sensor.value,
                    minimum: _memberBy(minDev?.sensors, 'name', sensor.name)?.value,
                    maximum: _memberBy(maxDev?.sensors, 'name', sensor.name)?.value,
                }))
                : hasTemp ? [{
                    name: 'Temperature',
                    value: dev.temp,
                    minimum: minDev?.temp,
                    maximum: maxDev?.temp,
                }] : [];
            const hasHistory = chart.data.datasets.some(hasEnvelopeData);
            if (readings.length > 0 || hasHistory) {
                const byName = new Map(readings.map(reading => [reading.name, reading]));
                const datasets = ensureSensorDatasets(
                    chart,
                    readings.map(reading => reading.name),
                    pairs,
                    ts,
                );
                chart._sensorNames = datasets.map(dataset => dataset.$kulaSensorName || dataset.label);
                datasets.forEach(dataset => {
                    const reading = byName.get(dataset.$kulaSensorName || dataset.label);
                    push(dataset, reading?.value ?? null, reading?.minimum, reading?.maximum);
                });
            }
        }
    }

    // GPU
    if (state.splitGpu && s.gpu?.length > 0 && state.splitCharts.gpu) {
        for (const g of s.gpu) {
            const minGPU = _gpuMember(minimum?.gpu, g);
            const maxGPU = _gpuMember(maximum?.gpu, g);
            const hasAny = g.load_pct > 0 || g.power_w > 0 || g.vram_total > 0 || g.temp > 0;
            if (!hasAny) continue;

            const safe = _sanitize(g.name);

            const loadChart = state.splitCharts.gpu[`gpuload_${g.name}`];
            if (loadChart?.data?.datasets && (g.load_pct > 0 || g.power_w > 0)) {
                setChartHidden(`card-split-gpuload-${safe}`, false);
                push(loadChart.data.datasets[0], g.load_pct || 0, minGPU?.load_pct, maxGPU?.load_pct);
                push(loadChart.data.datasets[1], g.power_w || 0, minGPU?.power_w, maxGPU?.power_w);
            }

            const vramChart = state.splitCharts.gpu[`vram_${g.name}`];
            if (vramChart?.data?.datasets && g.vram_total > 0 && g.vram_used > 0) {
                setChartHidden(`card-split-vram-${safe}`, false);
                push(vramChart.data.datasets[0], g.vram_used || 0, minGPU?.vram_used, maxGPU?.vram_used);
                vramChart.options.scales.y.max = g.vram_total > 0 ? g.vram_total : undefined;
            }

            const tempChart = state.splitCharts.gpu[`gputemp_${g.name}`];
            if (tempChart?.data?.datasets && g.temp > 0) {
                setChartHidden(`card-split-gputemp-${safe}`, false);
                setChartHidden('thermals-title', false);
                setChartHidden('thermals-grid', false);
                push(tempChart.data.datasets[0], g.temp, minGPU?.temp, maxGPU?.temp);
            }
        }
    }

    const activeSplitCharts = [
        [state.splitNet, state.splitCharts.network],
        [state.splitDiskIo, state.splitCharts.diskio],
        [state.splitDiskSpace, state.splitCharts.diskspace],
        [state.splitDiskTemp, state.splitCharts.disktemp],
        [state.splitGpu, state.splitCharts.gpu],
    ];
    activeSplitCharts.forEach(([enabled, charts]) => {
        if (!enabled || !charts) return;
        Object.values(charts).forEach(chart => {
            chart?.data?.datasets?.forEach(dataset => {
                if (!touchedDatasets.has(dataset) && dataset.data?.length > 0) {
                    appendEnvelopeGap(dataset, ts);
                }
            });
        });
    });
}

// ---- Private helpers ----

function getSplitState(type) {
    return !!state[SPLIT_STATE_KEY[type]];
}

function setSplitState(type, enabled) {
    state[SPLIT_STATE_KEY[type]] = enabled;
    localStorage.setItem(SPLIT_LS_KEY[type], JSON.stringify(enabled));
}

function _sanitize(str) {
    return String(str).replace(/[^a-zA-Z0-9_-]/g, '_');
}

function _memberBy(items, field, value) {
    return Array.isArray(items) ? items.find(item => item?.[field] === value) : undefined;
}

function _gpuMember(items, gpu) {
    if (!Array.isArray(items) || !gpu) return undefined;
    return items.find(item => item?.index === gpu.index) ||
        items.find(item => item?.name === gpu.name);
}

function _escapeHtml(str) {
    return String(str).replace(/[&<>"']/g, m => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[m]));
}

function _getOptions(type, s) {
    if (!s) {
        // fallback to cached state options
        switch (type) {
            case 'network':   return (state.netOptions || []).filter(n => n !== 'lo');
            case 'diskio':    return state.diskIoOptions || [];
            case 'diskspace': return state.diskSpaceOptions || [];
            case 'disktemp':  return state.diskTempOptions || [];
            case 'gpu':       return state.gpuLoadOptions || [];
        }
        return [];
    }
    switch (type) {
        case 'network':   return (s.net?.ifaces || []).map(i => i.name).filter(n => n !== 'lo').sort();
        case 'diskio':    return (s.disk?.devices || []).map(diskKey).sort();
        case 'diskspace': return (s.disk?.filesystems || []).map(f => f.mount).sort();
        case 'disktemp':  return (s.disk?.devices || []).filter(d => d.temp > 0 || (d.sensors && d.sensors.length > 0)).map(diskKey).sort();
        case 'gpu':       return (s.gpu || []).map(g => g.name).sort();
    }
    return [];
}

function _hideOriginalCards(type) {
    for (const id of SPLIT_ORIGINAL_CARDS[type]) {
        document.getElementById(id)?.classList.add('hidden');
    }
}

function _showOriginalCards(type) {
    for (const id of SPLIT_ORIGINAL_CARDS[type]) {
        const card = document.getElementById(id);
        if (card) card.classList.remove('hidden');
    }
}

function _triggerRedraw() {
    if (_redrawFromBuffer) {
        _rebuilding = true;
        _redrawFromBuffer();
        _rebuilding = false;
    }
}

function _clearDataForOptions(type, opts) {
    const charts = state.splitCharts[type];
    if (!charts) return;
    const keyMap = {
        network:   o => [`net_${o}`, `pps_${o}`],
        diskio:    o => [`diskio_${o}`],
        diskspace: o => [`diskspace_${o}`],
        disktemp:  o => [`disktemp_${o}`],
        gpu:       o => [`gpuload_${o}`, `vram_${o}`, `gputemp_${o}`],
    };
    const getKeys = keyMap[type];
    if (!getKeys) return;
    for (const opt of opts) {
        for (const key of getKeys(opt)) {
            const chart = charts[key];
            if (chart?.data?.datasets) {
                chart.data.datasets.forEach(clearEnvelopeData);
                queueChartUpdate(chart);
            }
        }
    }
}

function _addSplitButtons() {
    for (const [type, cardIds] of Object.entries(SPLIT_BTN_CARD)) {
        const ids = Array.isArray(cardIds) ? cardIds : [cardIds];
        ids.forEach((cardId, idx) => {
            const card = document.getElementById(cardId);
            if (!card) return;

            let actions = card.querySelector('.chart-header-right');
            if (!actions) {
                actions = document.createElement('div');
                actions.className = 'chart-header-right';
                const header = card.querySelector('.chart-header');
                if (header) header.appendChild(actions);
            }

            const btn = document.createElement('button');
            btn.className = 'btn-icon btn-split-chart';
            btn.dataset.splitBtnType = type;
            if (idx === 0) btn.id = `btn-split-${type}`;
            btn.title = i18n.t('split_by_device') || 'Split by device';
            btn.textContent = '⊟';
            btn.style.fontSize = '0.85rem';
            btn.style.padding = '0.15rem 0.35rem';
            btn.style.opacity = getSplitState(type) ? '1' : '0.5';
            btn.style.transition = 'opacity 0.15s';
            btn.onmouseenter = () => { btn.style.opacity = '1'; };
            btn.onmouseleave = () => { if (!getSplitState(type)) btn.style.opacity = '0.5'; };

            btn.addEventListener('click', (e) => {
                e.stopPropagation();
                _toggleSplit(type);
            });

            // Insert before other buttons so it appears first on the left
            actions.insertBefore(btn, actions.firstChild);
        });
    }
}

function _updateSplitBtn(type) {
    document.querySelectorAll(`[data-split-btn-type="${type}"]`).forEach(btn => {
        btn.style.opacity = getSplitState(type) ? '1' : '0.5';
    });
}

function _toggleSplit(type) {
    const newState = !getSplitState(type);
    setSplitState(type, newState);
    _updateSplitBtn(type);

    if (newState) {
        _enableSplit(type);
    } else {
        _disableSplit(type);
    }
}

function _enableSplit(type) {
    _hideOriginalCards(type);
    const options = _getOptions(type, null);
    if (options.length > 0) {
        _buildSplitChartsForType(type, options);
        splitOptionsCache[type] = options.join(',');
        _triggerRedraw();
    }
}

function _disableSplit(type) {
    _showOriginalCards(type);
    _destroySplitChartsForType(type);
    splitOptionsCache[type] = '';
    _triggerRedraw();
}

function _destroySplitChartsForType(type) {
    const charts = state.splitCharts[type];
    if (charts) {
        Object.values(charts).forEach(c => {
            if (c && typeof c.destroy === 'function') c.destroy();
        });
        state.splitCharts[type] = {};
    }
    document.querySelectorAll(`[data-split-type="${type}"]`).forEach(el => el.remove());
}

function _makeSplitCard(cardId, title, type, graphId = null) {
    const card = document.createElement('div');
    card.className = 'chart-card hidden';
    card.id = cardId;
    card.dataset.splitType = type;
    card.innerHTML = `
        <div class="chart-header">
            <h3>${_escapeHtml(title)}</h3>
            <div class="chart-header-right"></div>
        </div>
        <div class="chart-body"><canvas id="canvas-${_escapeHtml(cardId)}"></canvas></div>
    `;

    const header = card.querySelector('.chart-header');
    const actions = card.querySelector('.chart-header-right');
    header.style.position = 'relative';
    actions.style.marginLeft = 'auto';
    actions.style.display = 'flex';
    actions.style.alignItems = 'center';
    actions.style.gap = '0.35rem';

    // Join button — collapses split back to the combined chart
    const joinBtn = document.createElement('button');
    joinBtn.className = 'btn-icon btn-split-chart';
    joinBtn.title = i18n.t('join_charts') || 'Join charts';
    joinBtn.textContent = '⊞';
    joinBtn.style.fontSize = '0.85rem';
    joinBtn.style.padding = '0.15rem 0.35rem';
    joinBtn.style.opacity = '0.5';
    joinBtn.style.transition = 'opacity 0.15s';
    joinBtn.onmouseenter = () => { joinBtn.style.opacity = '1'; };
    joinBtn.onmouseleave = () => { joinBtn.style.opacity = '0.5'; };
    joinBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        _toggleSplit(type);
    });
    actions.appendChild(joinBtn);

    // Y-axis settings button (only for types that support Y-axis limits)
    if (graphId) {
        _addSettingsDropdown(header, actions, graphId, type);
    }

    // Expand button
    const expandBtn = document.createElement('button');
    expandBtn.className = 'btn-icon btn-expand-chart';
    expandBtn.title = 'Expand chart';
    expandBtn.textContent = '🔍';
    expandBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        _toggleExpandSplitCard(card);
    });
    actions.appendChild(expandBtn);

    // Hover pause
    card.addEventListener('mouseenter', () => {
        state.pausedHover = true;
        document.dispatchEvent(new Event('kula-sync-pause'));
    });
    card.addEventListener('mouseleave', () => {
        state.pausedHover = false;
        document.dispatchEvent(new Event('kula-sync-pause'));
    });

    return card;
}

function _toggleExpandSplitCard(card) {
    const grid = card.closest('.charts-grid');
    if (!grid) return;
    const visibleCards = Array.from(grid.querySelectorAll('.chart-card:not(.hidden)'));
    const isExpanding = !card.classList.contains('chart-expanded');

    if (isExpanding) {
        const hasAnyExpanded = visibleCards.some(c => c.classList.contains('chart-expanded'));
        if (!hasAnyExpanded) {
            visibleCards.forEach((c, idx) => { c.style.order = (idx + 1) * 10; });
        }
        const myTop = card.offsetTop;
        const sameRowCards = visibleCards.filter(c => Math.abs(c.offsetTop - myTop) < 10);
        if (sameRowCards.length > 0) {
            sameRowCards.sort((a, b) => a.offsetLeft - b.offsetLeft);
            const firstInRow = sameRowCards[0];
            const firstOrder = parseInt(firstInRow.style.order) || ((visibleCards.indexOf(firstInRow) + 1) * 10);
            card.style.order = firstOrder - 5;
        }
    }

    const isExpanded = card.classList.toggle('chart-expanded');

    if (!isExpanded) {
        const domIndex = visibleCards.indexOf(card);
        card.style.order = (domIndex + 1) * 10;
        const hasAnyExpanded = visibleCards.some(c => c.classList.contains('chart-expanded'));
        if (!hasAnyExpanded) {
            visibleCards.forEach(c => { c.style.order = ''; });
        }
    }

    const btn = card.querySelector('.btn-expand-chart');
    if (btn) btn.title = isExpanded ? 'Collapse chart' : 'Expand chart';

    const canvas = card.querySelector('canvas');
    if (canvas) {
        for (const typeCharts of Object.values(state.splitCharts)) {
            for (const chart of Object.values(typeCharts)) {
                if (chart && chart.canvas === canvas) {
                    setTimeout(() => chart.resize(), 50);
                    return;
                }
            }
        }
    }
}

function _addSettingsDropdown(header, actions, graphId, type) {
    const sBtn = document.createElement('button');
    sBtn.className = 'btn-icon';
    sBtn.title = 'Graph Bounds';
    sBtn.textContent = '⚙️';
    sBtn.style.fontSize = '0.85rem';
    sBtn.style.padding = '0.15rem 0.35rem';
    sBtn.style.opacity = '0.5';
    sBtn.style.transition = 'opacity 0.15s';
    sBtn.onmouseenter = () => { sBtn.style.opacity = '1'; };
    sBtn.onmouseleave = () => { sBtn.style.opacity = '0.5'; };

    const dropdown = document.createElement('div');
    dropdown.className = 'chart-settings-dropdown hidden';

    const titleEl = document.createElement('div');
    titleEl.style.marginBottom = '0.5rem';
    titleEl.style.fontSize = '0.75rem';
    titleEl.style.fontWeight = '600';
    titleEl.style.textTransform = 'uppercase';
    titleEl.style.color = 'var(--text-muted)';
    titleEl.textContent = 'Y-Axis Limit';

    const select = document.createElement('select');
    select.style.width = '100%';
    select.style.marginBottom = '0.5rem';
    select.style.padding = '0.3rem';
    select.style.borderRadius = 'var(--radius-sm)';
    select.style.border = '1px solid var(--border)';
    select.style.background = 'var(--bg-card)';
    select.style.color = 'var(--text)';
    select.style.fontSize = '0.85rem';
    select.innerHTML = `
        <option value="off">Off (Auto-scale)</option>
        <option value="on">On (Max Limit)</option>
    `;

    const input = document.createElement('input');
    input.type = 'number';
    input.placeholder = graphId === 'network' ? 'Mbps' : '°C';
    input.style.width = '100%';
    input.style.padding = '0.3rem';
    input.style.borderRadius = 'var(--radius-sm)';
    input.style.border = '1px solid var(--border)';
    input.style.background = 'var(--bg-card)';
    input.style.color = 'var(--text)';
    input.style.fontSize = '0.85rem';

    select.addEventListener('change', () => {
        input.style.display = select.value === 'off' ? 'none' : 'block';
    });

    const saveBtn = document.createElement('button');
    saveBtn.textContent = 'Apply';
    saveBtn.style.width = '100%';
    saveBtn.style.marginTop = '0.75rem';
    saveBtn.style.padding = '0.4rem';
    saveBtn.style.borderRadius = 'var(--radius-sm)';
    saveBtn.style.background = 'var(--accent-blue)';
    saveBtn.style.color = '#fff';
    saveBtn.style.border = 'none';
    saveBtn.style.cursor = 'pointer';
    saveBtn.style.fontSize = '0.85rem';
    saveBtn.style.fontWeight = '500';

    dropdown.appendChild(titleEl);
    dropdown.appendChild(select);
    dropdown.appendChild(input);
    dropdown.appendChild(saveBtn);

    header.appendChild(dropdown);
    actions.appendChild(sBtn);

    sBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        let prefs = {};
        try { prefs = JSON.parse(localStorage.getItem('kula_graphs_max') || '{}'); } catch (err) { }
        let cur = prefs[graphId] || (state.configMax && state.configMax[graphId]);
        // nosemgrep: insecure-object-assign -- assigned keys are static literals, not user-controlled (no mass assignment)
        if (!cur || !cur.mode) cur = Object.assign({}, cur, { mode: 'off', value: cur?.value || (graphId === 'network' ? 1000 : 100) });

        let uiMode = (cur.mode === 'auto' || cur.mode === 'on') ? 'on' : 'off';
        let uiVal = cur.value;
        if (cur.mode === 'auto') {
            uiVal = (typeof cur.auto === 'number' && cur.auto > 0) ? cur.auto : cur.value;
        }
        if (uiMode === 'off') {
            if (!uiVal) uiVal = (graphId === 'network') ? 1000 : 100;
            if (cur.auto && cur.auto > 0) uiVal = cur.auto;
        }

        select.value = uiMode;
        input.value = uiVal;
        input.style.display = uiMode === 'off' ? 'none' : 'block';

        document.querySelectorAll('.chart-settings-dropdown').forEach(d => {
            if (d !== dropdown) d.classList.add('hidden');
        });
        dropdown.classList.toggle('hidden');
    });

    select.addEventListener('click', e => e.stopPropagation());
    input.addEventListener('click', e => e.stopPropagation());
    titleEl.addEventListener('click', e => e.stopPropagation());
    dropdown.addEventListener('click', e => e.stopPropagation());

    saveBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        let prefs = {};
        try { prefs = JSON.parse(localStorage.getItem('kula_graphs_max') || '{}'); } catch (err) { }
        prefs[graphId] = {
            mode: select.value,
            value: parseFloat(input.value) || (graphId === 'network' ? 1000 : 100)
        };
        localStorage.setItem('kula_graphs_max', JSON.stringify(prefs));
        dropdown.classList.add('hidden');
        // Rebuild split charts for this type to apply the new Y-axis bound
        _rebuildSplitType(type);
    });

    document.addEventListener('click', (e) => {
        if (!dropdown.classList.contains('hidden') && !dropdown.contains(e.target) && e.target !== sBtn) {
            dropdown.classList.add('hidden');
        }
    });
}

function _rebuildSplitType(type) {
    const options = _getOptions(type, null);
    if (options.length > 0) {
        _buildSplitChartsForType(type, options);
        splitOptionsCache[type] = options.join(',');
        _triggerRedraw();
    }
}

function _insertCard(card, gridId, afterCardId) {
    const grid = document.getElementById(gridId);
    if (!grid) return;
    const after = document.getElementById(afterCardId);
    if (after && after.parentNode === grid) {
        after.insertAdjacentElement('afterend', card);
    } else {
        grid.appendChild(card);
    }
}

function _buildSplitChartsForType(type, options) {
    _destroySplitChartsForType(type);
    if (!state.splitCharts[type]) state.splitCharts[type] = {};
    const charts = state.splitCharts[type];

    if (type === 'network') {
        let prevId = 'card-pps';
        for (const iface of options) {
            const safe = _sanitize(iface);

            // Throughput card
            const netCardId = `card-split-net-${safe}`;
            const netCard = _makeSplitCard(netCardId, `${i18n.t('network_throughput')}: ${iface}`, type, 'network');
            _insertCard(netCard, 'charts-grid', prevId);
            prevId = netCardId;

            let yConf = { ticks: { callback: v => v.toFixed(1) + ' Mbps' } };
            const netMax = getChartMaxBound('network');
            if (netMax !== undefined) yConf.max = netMax;

            charts[`net_${iface}`] = createTimeSeriesChart(`canvas-${netCardId}`, [
                { label: i18n.t('rx'), borderColor: colors.cyan, backgroundColor: colors.cyanAlpha, fill: true, data: [] },
                { label: i18n.t('tx'), borderColor: colors.pink, backgroundColor: colors.pinkAlpha, fill: true, data: [] },
            ], yConf);
            if (charts[`net_${iface}`]) netCard.classList.remove('hidden');

            // PPS card
            const ppsCardId = `card-split-pps-${safe}`;
            const ppsCard = _makeSplitCard(ppsCardId, `${i18n.t('packets_sec')}: ${iface}`, type);
            _insertCard(ppsCard, 'charts-grid', prevId);
            prevId = ppsCardId;

            charts[`pps_${iface}`] = createTimeSeriesChart(`canvas-${ppsCardId}`, [
                { label: i18n.t('rx_pps'), borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
                { label: i18n.t('tx_pps'), borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true, data: [] },
            ], { ticks: { callback: v => formatPPS(v) } }, {
                tooltip: { callbacks: { label: ctx => ctx.dataset.label + ': ' + formatPPS(Math.round(ctx.parsed.y)) } }
            });
            if (charts[`pps_${iface}`]) ppsCard.classList.remove('hidden');
        }
    }

    if (type === 'diskio') {
        let prevId = 'card-disk-io';
        for (const dev of options) {
            const safe = diskDOMKey(dev);
            const cardId = `card-split-diskio-${safe}`;
            const card = _makeSplitCard(cardId, `${i18n.t('disk_io')}: ${diskLabel(state.diskDevices.get(dev))}`, type);
            card.querySelector('h3').title = diskTitle(state.diskDevices.get(dev));
            _insertCard(card, 'charts-grid', prevId);
            prevId = cardId;

            charts[`diskio_${dev}`] = createTimeSeriesChart(`canvas-${cardId}`, [
                { label: i18n.t('read_bs'), borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [], yAxisID: 'y' },
                { label: i18n.t('write_bs'), borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true, data: [], yAxisID: 'y' },
                { label: i18n.t('reads_s'), borderColor: colors.cyan, data: [], fill: false, borderDash: [4, 2], yAxisID: 'y1' },
                { label: i18n.t('writes_s'), borderColor: colors.pink, data: [], fill: false, borderDash: [4, 2], yAxisID: 'y1' },
            ], { ticks: { callback: v => formatBytesShort(v) + '/s' } }, {
                tooltip: { callbacks: { label: ctx => ctx.dataset.yAxisID === 'y1' ? ctx.dataset.label + ': ' + ctx.parsed.y.toFixed(0) + ' IOPS' : ctx.dataset.label + ': ' + formatBytesShort(Math.round(ctx.parsed.y)) + '/s' } }
            });
            const ch = charts[`diskio_${dev}`];
            if (ch) {
                ch.options.scales.y1 = { position: 'right', beginAtZero: true, grid: { display: false }, ticks: { callback: v => v.toFixed(0) + ' IO/s' } };
                queueChartUpdate(ch);
                card.classList.remove('hidden');
            }
        }
    }

    if (type === 'diskspace') {
        let prevId = 'card-disk-space';
        for (const mount of options) {
            const safe = _sanitize(mount);
            const cardId = `card-split-diskspace-${safe}`;
            const card = _makeSplitCard(cardId, `${i18n.t('disk_space')}: ${mount}`, type);
            _insertCard(card, 'charts-grid', prevId);
            prevId = cardId;

            charts[`diskspace_${mount}`] = createTimeSeriesChart(`canvas-${cardId}`, [
                { label: mount, borderColor: colors.purple, backgroundColor: colors.purpleAlpha, fill: true, data: [], pointHitRadius: 5 },
            ], { max: 100, ticks: { callback: v => Math.round(v) + '%' } }, {
                tooltip: { callbacks: { label: ctx => { const raw = ctx.raw; if (raw?.used !== undefined && raw?.total !== undefined) return `${ctx.dataset.label}: ${ctx.parsed.y.toFixed(1)}% (${formatBytesShort(raw.used)} / ${formatBytesShort(raw.total)})`; return ctx.dataset.label + ': ' + ctx.parsed.y.toFixed(1) + '%'; } } }
            });
            if (charts[`diskspace_${mount}`]) card.classList.remove('hidden');
        }
    }

    if (type === 'disktemp') {
        let prevId = 'card-disk-temp';
        let diskTempMax = getChartMaxBound('disk_temp');
        let diskTempYConf = { ticks: { callback: v => v.toFixed(1) + '°C' } };
        if (diskTempMax !== undefined) diskTempYConf.max = diskTempMax;

        for (const dev of options) {
            const safe = diskDOMKey(dev);
            const cardId = `card-split-disktemp-${safe}`;
            const card = _makeSplitCard(cardId, `${i18n.t('disk_temp')}: ${diskLabel(state.diskDevices.get(dev))}`, type, 'disk_temp');
            card.querySelector('h3').title = diskTitle(state.diskDevices.get(dev));
            _insertCard(card, 'thermals-grid', prevId);
            prevId = cardId;

            charts[`disktemp_${dev}`] = createTimeSeriesChart(`canvas-${cardId}`, [
                { label: i18n.t('temperature'), borderColor: colors.red, backgroundColor: colors.redAlpha, fill: true, data: [] },
            ], diskTempYConf);
            // Card visibility is controlled in addSampleToSplitCharts when data arrives
        }
    }

    if (type === 'gpu') {
        let prevMainId = 'card-vram';
        let prevThermId = 'card-gpu-temp';
        let gpuTempMax = getChartMaxBound('gpu_temp');
        let gpuTempYConf = { max: gpuTempMax, ticks: { callback: v => v.toFixed(1) + '°C' } };

        for (const gpu of options) {
            const safe = _sanitize(gpu);

            // GPU Load card (in main grid)
            const loadCardId = `card-split-gpuload-${safe}`;
            const loadCard = _makeSplitCard(loadCardId, `${i18n.t('gpu_load')}: ${gpu}`, type);
            _insertCard(loadCard, 'charts-grid', prevMainId);
            prevMainId = loadCardId;

            charts[`gpuload_${gpu}`] = createTimeSeriesChart(`canvas-${loadCardId}`, [
                { label: i18n.t('load_pct'), borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
                { label: i18n.t('power_w'), borderColor: colors.orange, data: [], fill: false, yAxisID: 'y1' },
            ], { max: 100, ticks: { callback: v => formatMetricNumber(v) + '%' } });
            const loadCh = charts[`gpuload_${gpu}`];
            if (loadCh) {
                loadCh.options.scales.y1 = { position: 'right', beginAtZero: true, grid: { display: false }, ticks: { callback: v => v.toFixed(1) + ' W' } };
                queueChartUpdate(loadCh);
            }

            // VRAM card (in main grid)
            const vramCardId = `card-split-vram-${safe}`;
            const vramCard = _makeSplitCard(vramCardId, `${i18n.t('vram_usage')}: ${gpu}`, type);
            _insertCard(vramCard, 'charts-grid', prevMainId);
            prevMainId = vramCardId;

            charts[`vram_${gpu}`] = createTimeSeriesChart(`canvas-${vramCardId}`, [
                { label: i18n.t('used'), borderColor: colors.purple, backgroundColor: colors.purpleAlpha, fill: true, data: [] },
            ], { ticks: { callback: v => formatBytesShort(v) } }, {
                tooltip: { callbacks: { label: ctx => ctx.dataset.label + ': ' + formatBytesShort(ctx.parsed.y) } }
            });

            // GPU Temp card (in thermals grid)
            const tempCardId = `card-split-gputemp-${safe}`;
            const tempCard = _makeSplitCard(tempCardId, `${i18n.t('gpu_temp')}: ${gpu}`, type, 'gpu_temp');
            _insertCard(tempCard, 'thermals-grid', prevThermId);
            prevThermId = tempCardId;

            charts[`gputemp_${gpu}`] = createTimeSeriesChart(`canvas-${tempCardId}`, [
                { label: i18n.t('temperature'), borderColor: colors.red, backgroundColor: colors.redAlpha, fill: true, data: [] },
            ], gpuTempYConf);
            // Visibility controlled in addSampleToSplitCharts
        }
    }
}
