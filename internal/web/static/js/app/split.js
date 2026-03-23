/* ============================================================
   split.js — Per-device/interface graph splitting.
   Allows splitting multi-device charts into individual charts,
   configurable from the dashboard or config file.
   ============================================================ */
'use strict';
import { state, colors, getChartMaxBound } from './state.js';
import { createTimeSeriesChart } from './charts-init.js';
import { formatBytesShort, formatPPS } from './utils.js';
import { i18n } from './i18n.js';

let _redrawFromBuffer = null;
let _rebuilding = false;

// Cards that receive a split toggle button
const SPLIT_BTN_CARD = {
    network:   'card-network',
    diskio:    'card-disk-io',
    diskspace: 'card-disk-space',
    disktemp:  'card-disk-temp',
    gpu:       'card-gpu-load',
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
            splitOptionsCache[type] = optKey;
            _hideOriginalCards(type);
            _buildSplitChartsForType(type, options);
            _triggerRedraw();
        }
    }
}

// Called from addSampleToCharts in charts-data.js for every data point
export function addSampleToSplitCharts(s, ts) {
    const point = v => ({ x: ts, y: v });

    // Network
    if (state.splitNet && s.net?.ifaces && state.splitCharts.network) {
        for (const iface of s.net.ifaces) {
            if (iface.name === 'lo') continue;
            const netChart = state.splitCharts.network[`net_${iface.name}`];
            const ppsChart = state.splitCharts.network[`pps_${iface.name}`];
            if (netChart?.data?.datasets) {
                netChart.data.datasets[0].data.push(point(iface.rx_mbps || 0));
                netChart.data.datasets[1].data.push(point(iface.tx_mbps || 0));
            }
            if (ppsChart?.data?.datasets) {
                ppsChart.data.datasets[0].data.push(point(iface.rx_pps || 0));
                ppsChart.data.datasets[1].data.push(point(iface.tx_pps || 0));
            }
        }
    }

    // Disk I/O
    if (state.splitDiskIo && s.disk?.devices && state.splitCharts.diskio) {
        for (const dev of s.disk.devices) {
            const chart = state.splitCharts.diskio[`diskio_${dev.name}`];
            if (chart?.data?.datasets) {
                chart.data.datasets[0].data.push(point(dev.read_bps || 0));
                chart.data.datasets[1].data.push(point(dev.write_bps || 0));
                chart.data.datasets[2].data.push(point(dev.reads_ps || 0));
                chart.data.datasets[3].data.push(point(dev.writes_ps || 0));
            }
        }
    }

    // Disk Space
    if (state.splitDiskSpace && s.disk?.filesystems && state.splitCharts.diskspace) {
        for (const fs of s.disk.filesystems) {
            const chart = state.splitCharts.diskspace[`diskspace_${fs.mount}`];
            if (chart?.data?.datasets) {
                chart.data.datasets[0].data.push({ x: ts, y: fs.used_pct || 0, used: fs.used || 0, total: fs.total || 0 });
            }
        }
    }

    // Disk Temp
    if (state.splitDiskTemp && s.disk?.devices && state.splitCharts.disktemp) {
        const thermalsTitle = document.getElementById('thermals-title');
        const thermalsGrid  = document.getElementById('thermals-grid');
        for (const dev of s.disk.devices) {
            const hasSensors = dev.sensors && dev.sensors.length > 0;
            const hasTemp    = dev.temp > 0;
            if (!hasSensors && !hasTemp) continue;

            const card = document.getElementById(`card-split-disktemp-${_sanitize(dev.name)}`);
            if (card) {
                card.classList.remove('hidden');
                thermalsTitle?.classList.remove('hidden');
                thermalsGrid?.classList.remove('hidden');
            }

            if (hasSensors) {
                // Multi-sensor device — one dataset per sensor
                const incomingNames = dev.sensors.map(sens => sens.name);
                const chart = state.splitCharts.disktemp[`disktemp_${dev.name}`];
                if (chart) {
                    if (incomingNames.join(',') !== (chart._sensorNames || []).join(',')) {
                        chart._sensorNames = incomingNames;
                        const pairs = [
                            [colors.red, colors.redAlpha],
                            [colors.orange, colors.orangeAlpha],
                            [colors.yellow, colors.yellowAlpha],
                            [colors.pink, colors.pinkAlpha],
                            [colors.purple, colors.purpleAlpha],
                            [colors.cyan, colors.cyanAlpha],
                        ];
                        chart.data.datasets = incomingNames.map((name, i) => ({
                            label: name,
                            borderColor: pairs[i % pairs.length][0],
                            backgroundColor: pairs[i % pairs.length][1],
                            fill: i === 0,
                            data: [],
                            pointHitRadius: 5,
                        }));
                    }
                    dev.sensors.forEach((sens, i) => {
                        if (i < chart.data.datasets.length) {
                            chart.data.datasets[i].data.push(point(sens.value));
                        }
                    });
                }
            } else {
                const chart = state.splitCharts.disktemp[`disktemp_${dev.name}`];
                if (chart?.data?.datasets?.[0]) {
                    chart.data.datasets[0].data.push(point(dev.temp));
                }
            }
        }
    }

    // GPU
    if (state.splitGpu && s.gpu?.length > 0 && state.splitCharts.gpu) {
        const thermalsTitle = document.getElementById('thermals-title');
        const thermalsGrid  = document.getElementById('thermals-grid');
        for (const g of s.gpu) {
            const hasAny = g.load_pct > 0 || g.power_w > 0 || g.vram_total > 0 || g.temp > 0;
            if (!hasAny) continue;

            const safe = _sanitize(g.name);
            const loadCard = document.getElementById(`card-split-gpuload-${safe}`);
            const vramCard = document.getElementById(`card-split-vram-${safe}`);
            const tempCard = document.getElementById(`card-split-gputemp-${safe}`);

            const loadChart = state.splitCharts.gpu[`gpuload_${g.name}`];
            if (loadChart?.data?.datasets && (g.load_pct > 0 || g.power_w > 0)) {
                loadCard?.classList.remove('hidden');
                loadChart.data.datasets[0].data.push(point(g.load_pct || 0));
                loadChart.data.datasets[1].data.push(point(g.power_w || 0));
            }

            const vramChart = state.splitCharts.gpu[`vram_${g.name}`];
            if (vramChart?.data?.datasets && g.vram_total > 0 && g.vram_used > 0) {
                vramCard?.classList.remove('hidden');
                vramChart.data.datasets[0].data.push(point(g.vram_used || 0));
                vramChart.options.scales.y.max = g.vram_total > 0 ? g.vram_total : undefined;
            }

            const tempChart = state.splitCharts.gpu[`gputemp_${g.name}`];
            if (tempChart?.data?.datasets && g.temp > 0) {
                tempCard?.classList.remove('hidden');
                thermalsTitle?.classList.remove('hidden');
                thermalsGrid?.classList.remove('hidden');
                tempChart.data.datasets[0].data.push(point(g.temp));
            }
        }
    }
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
        case 'diskio':    return (s.disk?.devices || []).map(d => d.name).sort();
        case 'diskspace': return (s.disk?.filesystems || []).map(f => f.mount).sort();
        case 'disktemp':  return (s.disk?.devices || []).filter(d => d.temp > 0 || (d.sensors && d.sensors.length > 0)).map(d => d.name).sort();
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

function _addSplitButtons() {
    for (const [type, cardId] of Object.entries(SPLIT_BTN_CARD)) {
        const card = document.getElementById(cardId);
        if (!card) continue;

        let actions = card.querySelector('.chart-header-right');
        if (!actions) {
            actions = document.createElement('div');
            actions.className = 'chart-header-right';
            const header = card.querySelector('.chart-header');
            if (header) header.appendChild(actions);
        }

        const btn = document.createElement('button');
        btn.className = 'btn-icon btn-split-chart';
        btn.id = `btn-split-${type}`;
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
    }
}

function _updateSplitBtn(type) {
    const btn = document.getElementById(`btn-split-${type}`);
    if (btn) btn.style.opacity = getSplitState(type) ? '1' : '0.5';
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

function _makeSplitCard(cardId, title, type) {
    const card = document.createElement('div');
    card.className = 'chart-card hidden';
    card.id = cardId;
    card.dataset.splitType = type;
    card.innerHTML = `
        <div class="chart-header">
            <h3>${_escapeHtml(title)}</h3>
        </div>
        <div class="chart-body"><canvas id="canvas-${_escapeHtml(cardId)}"></canvas></div>
    `;
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
            const netCard = _makeSplitCard(netCardId, `${i18n.t('network_throughput')}: ${iface}`, type);
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
            const safe = _sanitize(dev);
            const cardId = `card-split-diskio-${safe}`;
            const card = _makeSplitCard(cardId, `${i18n.t('disk_io')}: ${dev}`, type);
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
                ch.update('none');
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
            const safe = _sanitize(dev);
            const cardId = `card-split-disktemp-${safe}`;
            const card = _makeSplitCard(cardId, `${i18n.t('disk_temp')}: ${dev}`, type);
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
            ], { max: 100, ticks: { callback: v => v + '%' } });
            const loadCh = charts[`gpuload_${gpu}`];
            if (loadCh) {
                loadCh.options.scales.y1 = { position: 'right', beginAtZero: true, grid: { display: false }, ticks: { callback: v => v.toFixed(1) + ' W' } };
                loadCh.update('none');
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
            const tempCard = _makeSplitCard(tempCardId, `${i18n.t('gpu_temp')}: ${gpu}`, type);
            _insertCard(tempCard, 'thermals-grid', prevThermId);
            prevThermId = tempCardId;

            charts[`gputemp_${gpu}`] = createTimeSeriesChart(`canvas-${tempCardId}`, [
                { label: i18n.t('temperature'), borderColor: colors.red, backgroundColor: colors.redAlpha, fill: true, data: [] },
            ], gpuTempYConf);
            // Visibility controlled in addSampleToSplitCharts
        }
    }
}
