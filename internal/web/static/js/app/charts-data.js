/* ============================================================
   charts-data.js — Chart data ingestion, updates, zoom sync,
   gap insertion, device selectors, and the live sample pipeline.
   ============================================================ */
'use strict';
import { state, colors } from './state.js';
import { diskKey, diskMember, diskLabel, diskTitle, migrateDiskSelection } from './disk-identity.js';
import { formatBytesShort, formatMetricNumber, formatRangeTimestamp } from './format.js';
import { createTimeSeriesChart, setChartTimeRange, updateChartLabels } from './charts-init.js';
import { updateHeader, updateSubtitles } from './header.js';
import { updateGauges } from './gauges.js';
import { evaluateAlerts } from './alerts.js';
import { applyStoredFocusMode } from './focus-mode.js';
import { addSampleToSplitCharts, updateSplitSelectors } from './split.js';
import { attachDynamicChartCardActions } from './chart-card-actions.js';
import {
    addContainerSample,
    markContainersAbsent,
} from './container-apps.js';
import { apiUrl } from './api.js';
import {
    HistoryRequestController,
    liveHistoryRefreshInterval,
    updateLiveSampleInterval,
} from './history-request.js';
import {
    aggregationField,
    annotateHistoryItems,
    historyItemContext,
    historyItemSample,
    historyItemTimestamp,
    historySectionsForFocus,
    insertHistoryGaps,
    normalizeHistoryItem,
    resolveAggregation,
} from './history-data.js';
import { i18n } from './i18n.js';
import {
    forEachRegisteredChart,
    historyPointBudget,
    queueAllChartUpdates,
    queueChartUpdate,
} from './chart-controller.js';
import { clampHistoryInterval, fitZoomToObservations, minimumZoomSpan } from './history-navigation.js';
import {
    appendEnvelopeGap,
    appendEnvelopePoint,
    clearEnvelopeData,
    ensureSensorDatasets,
    hasEnvelopeData,
    trimEnvelopeData,
} from './chart-envelope.js';
import { updateChartAccessibility } from './chart-accessibility.js';
import { batchChartUI, updateChartUI, setChartHidden, setChartSubtitle } from './chart-ui.js';

const historyRequests = new HistoryRequestController();

// CSS order values for dynamic app chart grouping within the grid.
const APP_ORDER_NGINX = 10;
const APP_ORDER_APACHE2 = 15;
const APP_ORDER_POSTGRES = 30;
const APP_ORDER_MYSQL = 38;
const APP_ORDER_CUSTOM = 50;

// createAppChartCard creates a chart-card DOM structure in the applications
// grid and returns the canvas ID for use with createTimeSeriesChart.
function createAppChartCard(cardId, chartId, subtitleId, title, order) {
    const grid = document.getElementById('applications-grid');
    if (!grid) return null;

    const wrapper = document.createElement('div');
    wrapper.className = 'chart-card';
    wrapper.id = cardId;
    wrapper.dataset.appChart = '';
    wrapper.style.order = order;

    const header = document.createElement('div');
    header.className = 'chart-header';
    const h3 = document.createElement('h3');
    h3.textContent = title;
    const span = document.createElement('span');
    span.className = 'chart-subtitle';
    span.id = subtitleId;
    header.appendChild(h3);
    header.appendChild(span);

    const body = document.createElement('div');
    body.className = 'chart-body';
    const canvas = document.createElement('canvas');
    canvas.id = chartId;
    body.appendChild(canvas);

    wrapper.appendChild(header);
    wrapper.appendChild(body);

    // setupChartActions only runs at page load, so dynamic app cards need
    // their expand and hover-pause interactions wired here. Zoom reset uses
    // main.js's delegated canvas double-click handler.
    attachDynamicChartCardActions(wrapper);

    // If focus mode is active, place card in the combined grid and apply visibility
    if (state.focusMode && !state.focusSelecting && state.focusVisible) {
        const mainGrid = document.getElementById('charts-grid');
        (mainGrid || grid).appendChild(wrapper);
        wrapper.classList.toggle('focus-visible', state.focusVisible.includes(cardId));
    } else if (state.focusSelecting) {
        grid.appendChild(wrapper);
        if (state.focusVisible?.includes(cardId)) wrapper.classList.add('focus-selected');
        wrapper._focusClick = () => wrapper.classList.toggle('focus-selected');
        wrapper.addEventListener('click', wrapper._focusClick);
    } else {
        grid.appendChild(wrapper);
    }

    return chartId;
}

// Helper: iterate all dynamic chart instances (for utility functions).
function forEachAppChart(fn) {
    Object.values(state.containerCharts || {}).forEach(chart => { if (chart) fn(chart); });
    Object.values(state.customCharts || {}).forEach(entry => { if (entry?.chart) fn(entry.chart); });
    Object.values(state.psuCharts || {}).forEach(chart => { if (chart) fn(chart); });
}

function rebuildHistoryPointContexts() {
    const contexts = new Map();
    state.dataBuffer.forEach(item => {
        const timestamp = new Date(historyItemTimestamp(item)).getTime();
        const context = historyItemContext(item);
        if (Number.isFinite(timestamp) && context) contexts.set(timestamp, context);
    });
    state.historyPointContexts = contexts;
}

function memberBy(items, field, value) {
    return Array.isArray(items) ? items.find(item => item?.[field] === value) : undefined;
}

function gpuMember(items, gpu) {
    if (!Array.isArray(items) || !gpu) return undefined;
    const byIndex = items.find(item => item?.index === gpu.index);
    return byIndex || items.find(item => item?.name === gpu.name);
}

// ---- Data Update ----
export function addSampleToCharts(item, ts, {
    aggregation = state.currentAggregation,
    validAggregations = state.validAggregations,
    charts = null,
} = {}) {
    ts = ts instanceof Date ? ts.getTime() : ts;
    if (!Number.isFinite(ts)) return;
    const historyItem = normalizeHistoryItem(item);
    let s = historyItemSample(historyItem);
    if (!s) return;
    const selectedField = aggregationField(aggregation);
    if (validAggregations.includes(selectedField) && historyItem[selectedField]) {
        s = historyItem[selectedField];
    }

    const hasEnvelope = validAggregations.includes('min') &&
        validAggregations.includes('max') && !!historyItem.min && !!historyItem.max;
    const minimum = hasEnvelope ? historyItem.min : null;
    const maximum = hasEnvelope ? historyItem.max : null;
    const touchedDatasets = new Set();
    const wantsChart = chart => chart && (!charts || charts.has(chart));

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
    const pushFields = (chart, value, minValue, maxValue, fields) => {
        fields.forEach((field, index) => {
            push(chart.data.datasets[index], value?.[field], minValue?.[field], maxValue?.[field]);
        });
    };

    // CPU
    if (wantsChart(state.charts.cpu) && s.cpu?.total) {
        pushFields(
            state.charts.cpu,
            s.cpu.total,
            minimum?.cpu?.total,
            maximum?.cpu?.total,
            ['user', 'system', 'iowait', 'steal', 'usage'],
        );
    }

    // CPU Temperature
    if (wantsChart(state.charts.cputemp)) {
        const hasSensors = Array.isArray(s.cpu?.sensors) && s.cpu.sensors.length > 0;
        const readings = hasSensors
            ? s.cpu.sensors.map(sensor => ({
                name: sensor.name,
                value: sensor.value,
                minimum: memberBy(minimum?.cpu?.sensors, 'name', sensor.name)?.value,
                maximum: memberBy(maximum?.cpu?.sensors, 'name', sensor.name)?.value,
            }))
            : s.cpu?.temp > 0 ? [{
                name: 'Temperature',
                value: s.cpu.temp,
                minimum: minimum?.cpu?.temp,
                maximum: maximum?.cpu?.temp,
            }] : [];
        const hasHistory = state.charts.cputemp.data.datasets.some(hasEnvelopeData);
        if (readings.length > 0) {
            setChartHidden('card-cpu-temp', false);
            setChartHidden('thermals-title', false);
            setChartHidden('thermals-grid', false);
        }
        if (readings.length > 0 || hasHistory) {
            const cpuTempColorPairs = [
                [colors.orange, colors.orangeAlpha],
                [colors.red, colors.redAlpha],
                [colors.yellow, colors.yellowAlpha],
                [colors.pink, colors.pinkAlpha],
                [colors.purple, colors.purpleAlpha],
                [colors.cyan, colors.cyanAlpha],
            ];
            const byName = new Map(readings.map(reading => [reading.name, reading]));
            const datasets = ensureSensorDatasets(
                state.charts.cputemp,
                readings.map(reading => reading.name),
                cpuTempColorPairs,
                ts,
            );
            state.cpuTempSensorNames = datasets.map(dataset => dataset.$kulaSensorName || dataset.label);
            datasets.forEach(dataset => {
                const reading = byName.get(dataset.$kulaSensorName || dataset.label);
                push(dataset, reading?.value ?? null, reading?.minimum, reading?.maximum);
            });
        }
    }

    // Load Average
    if (wantsChart(state.charts.loadavg) && s.lavg) {
        pushFields(state.charts.loadavg, s.lavg, minimum?.lavg, maximum?.lavg, ['load1', 'load5', 'load15']);
    }

    // Memory — with Free, Available, and Shmem
    if (wantsChart(state.charts.memory) && s.mem) {
        pushFields(
            state.charts.memory,
            s.mem,
            minimum?.mem,
            maximum?.mem,
            ['used', 'buffers', 'cached', 'shmem', 'free', 'available'],
        );
        // Set max to total RAM
        if (s.mem.total > 0) {
            state.charts.memory.options.scales.y.max = s.mem.total;
        }
    }

    // Swap — with Free
    if (wantsChart(state.charts.swap) && s.swap) {
        pushFields(state.charts.swap, s.swap, minimum?.swap, maximum?.swap, ['used', 'free']);
        // Set max to total swap
        if (s.swap.total > 0) {
            state.charts.swap.options.scales.y.max = s.swap.total;
        }
    }

    // Network (selected non-lo interface) — skip when split is active
    if (!state.splitNet && wantsChart(state.charts.network) && s.net?.ifaces) {
        let rx = 0, tx = 0;
        const iface = s.net.ifaces.find(i => i.name === state.selectedNet);
        let minIface, maxIface;
        if (iface) {
            rx = iface.rx_mbps || 0;
            tx = iface.tx_mbps || 0;
            minIface = memberBy(minimum?.net?.ifaces, 'name', iface.name);
            maxIface = memberBy(maximum?.net?.ifaces, 'name', iface.name);
        } else if (!state.selectedNet && s.net.ifaces.length > 0) {
            // Sum all if nothing selected
            s.net.ifaces.forEach(i => { if (i.name !== 'lo') { rx += i.rx_mbps || 0; tx += i.tx_mbps || 0; } });
        }
        push(state.charts.network.data.datasets[0], rx, minIface?.rx_mbps, maxIface?.rx_mbps);
        push(state.charts.network.data.datasets[1], tx, minIface?.tx_mbps, maxIface?.tx_mbps);
    }

    // Packets per second (selected non-lo interface) — skip when split is active
    if (!state.splitNet && wantsChart(state.charts.pps) && s.net?.ifaces) {
        let rxPps = 0, txPps = 0;
        const iface = s.net.ifaces.find(i => i.name === state.selectedNet);
        let minIface, maxIface;
        if (iface) {
            rxPps = iface.rx_pps || 0;
            txPps = iface.tx_pps || 0;
            minIface = memberBy(minimum?.net?.ifaces, 'name', iface.name);
            maxIface = memberBy(maximum?.net?.ifaces, 'name', iface.name);
        } else if (!state.selectedNet && s.net.ifaces.length > 0) {
            s.net.ifaces.forEach(i => { if (i.name !== 'lo') { rxPps += i.rx_pps || 0; txPps += i.tx_pps || 0; } });
        }
        push(state.charts.pps.data.datasets[0], rxPps, minIface?.rx_pps, maxIface?.rx_pps);
        push(state.charts.pps.data.datasets[1], txPps, minIface?.tx_pps, maxIface?.tx_pps);
    }

    // Connections
    if (wantsChart(state.charts.connections) && s.net?.sockets) {
        const datasets = state.charts.connections.data.datasets;
        push(datasets[0], s.net.sockets.tcp_inuse, minimum?.net?.sockets?.tcp_inuse, maximum?.net?.sockets?.tcp_inuse);
        push(datasets[1], s.net.sockets.udp_inuse, minimum?.net?.sockets?.udp_inuse, maximum?.net?.sockets?.udp_inuse);
        push(datasets[2], s.net.sockets.tcp_tw, minimum?.net?.sockets?.tcp_tw, maximum?.net?.sockets?.tcp_tw);
        push(datasets[3], s.net?.tcp?.curr_estab || 0, minimum?.net?.tcp?.curr_estab, maximum?.net?.tcp?.curr_estab);
        push(datasets[4], s.net?.tcp?.in_errs_ps || 0, minimum?.net?.tcp?.in_errs_ps, maximum?.net?.tcp?.in_errs_ps);
        push(datasets[5], s.net?.tcp?.out_rsts_ps || 0, minimum?.net?.tcp?.out_rsts_ps, maximum?.net?.tcp?.out_rsts_ps);
        push(datasets[6], s.net?.tcp?.retrans_ps || 0, minimum?.net?.tcp?.retrans_ps, maximum?.net?.tcp?.retrans_ps);
    }

    // Disk I/O (selected device) — skip when split is active
    if (!state.splitDiskIo && wantsChart(state.charts.diskio)) {
        let rBps = 0, wBps = 0, rIops = 0, wIops = 0;
        const d = diskMember(s.disk?.devices, state.selectedDiskIo);
        let minDisk, maxDisk;
        if (d) {
            rBps = d.read_bps || 0;
            wBps = d.write_bps || 0;
            rIops = d.reads_ps || 0;
            wIops = d.writes_ps || 0;
            minDisk = diskMember(minimum?.disk?.devices, diskKey(d));
            maxDisk = diskMember(maximum?.disk?.devices, diskKey(d));
        } else if (!state.selectedDiskIo && s.disk?.devices?.length > 0) {
            s.disk.devices.forEach(d => {
                rBps += d.read_bps || 0;
                wBps += d.write_bps || 0;
                rIops += d.reads_ps || 0;
                wIops += d.writes_ps || 0;
            });
        } else {
            rBps = wBps = rIops = wIops = null;
        }
        const datasets = state.charts.diskio.data.datasets;
        push(datasets[0], rBps, minDisk?.read_bps, maxDisk?.read_bps);
        push(datasets[1], wBps, minDisk?.write_bps, maxDisk?.write_bps);
        push(datasets[2], rIops, minDisk?.reads_ps, maxDisk?.reads_ps);
        push(datasets[3], wIops, minDisk?.writes_ps, maxDisk?.writes_ps);
    }

    // Disk Temperature — skip when split is active
    if (!state.splitDiskTemp && wantsChart(state.charts.disktemp)) {
        const d = diskMember(s.disk?.devices, state.selectedDiskTemp);
        const minDisk = diskMember(minimum?.disk?.devices, diskKey(d));
        const maxDisk = diskMember(maximum?.disk?.devices, diskKey(d));
        const hasSensors = d && d.sensors && d.sensors.length > 0;
        const hasTemp = d && d.temp > 0;

        if (hasSensors || hasTemp) {
            setChartHidden('card-disk-temp', false);
            setChartHidden('thermals-title', false);
            setChartHidden('thermals-grid', false);
        }

        const readings = hasSensors
            ? d.sensors.map(sensor => ({
                name: sensor.name,
                value: sensor.value,
                minimum: memberBy(minDisk?.sensors, 'name', sensor.name)?.value,
                maximum: memberBy(maxDisk?.sensors, 'name', sensor.name)?.value,
            }))
            : hasTemp ? [{
                name: 'Temperature',
                value: d.temp,
                minimum: minDisk?.temp,
                maximum: maxDisk?.temp,
            }] : [];
        const hasHistory = state.charts.disktemp.data.datasets.some(hasEnvelopeData);
        if (readings.length > 0 || hasHistory) {
            const tempColorPairs = [
                [colors.red, colors.redAlpha],
                [colors.orange, colors.orangeAlpha],
                [colors.yellow, colors.yellowAlpha],
                [colors.pink, colors.pinkAlpha],
                [colors.purple, colors.purpleAlpha],
                [colors.cyan, colors.cyanAlpha],
            ];
            const byName = new Map(readings.map(reading => [reading.name, reading]));
            const datasets = ensureSensorDatasets(
                state.charts.disktemp,
                readings.map(reading => reading.name),
                tempColorPairs,
                ts,
            );
            state.diskTempSensorNames = datasets.map(dataset => dataset.$kulaSensorName || dataset.label);
            datasets.forEach(dataset => {
                const reading = byName.get(dataset.$kulaSensorName || dataset.label);
                push(dataset, reading?.value ?? null, reading?.minimum, reading?.maximum);
            });
        }
    }

    // Disk Space — single dataset for selected mount — skip when split is active
    if (!state.splitDiskSpace && wantsChart(state.charts.diskspace) && s.disk?.filesystems && s.disk.filesystems.length > 0) {
        if ((state.charts.diskspace.data.datasets.length !== 1 || state.charts.diskspace.data.datasets[0].label !== 'Space Used %')) {
            state.charts.diskspace.data.datasets = [{
                label: 'Space Used %',
                borderColor: colors.purple,
                backgroundColor: colors.purpleAlpha,
                fill: false,
                tension: 0,
                data: [],
                pointHitRadius: 5,
            }];
        }
        let usedPct = 0, used = 0, total = 0;
        const fs = s.disk.filesystems.find(f => f.mount === state.selectedDiskSpace);
        let minFS, maxFS;
        if (fs) {
            usedPct = fs.used_pct || 0;
            used = fs.used || 0;
            total = fs.total || 0;
            minFS = memberBy(minimum?.disk?.filesystems, 'mount', fs.mount);
            maxFS = memberBy(maximum?.disk?.filesystems, 'mount', fs.mount);
        } else if (!state.selectedDiskSpace) {
            s.disk.filesystems.forEach(f => { used += f.used || 0; total += f.total || 0; });
            if (total > 0) usedPct = (used / total) * 100;
        }
        push(state.charts.diskspace.data.datasets[0], usedPct, minFS?.used_pct, maxFS?.used_pct, { used, total });
    }

    // Processes
    if (wantsChart(state.charts.processes) && s.proc) {
        pushFields(
            state.charts.processes,
            s.proc,
            minimum?.proc,
            maximum?.proc,
            ['running', 'sleeping', 'blocked', 'zombie', 'total'],
        );
    }

    // Entropy
    if (wantsChart(state.charts.entropy) && s.sys) {
        push(state.charts.entropy.data.datasets[0], s.sys.entropy, minimum?.sys?.entropy, maximum?.sys?.entropy);
    }

    // Self
    if (wantsChart(state.charts.self) && s.self) {
        pushFields(state.charts.self, s.self, minimum?.self, maximum?.self, ['cpu_pct', 'mem_rss']);
    }

    // GPU Metrics — skip regular cards when split is active
    const updateGPU = !state.splitGpu && (!charts ||
        [state.charts.gpuload, state.charts.vram, state.charts.gputemp].some(wantsChart));
    if (updateGPU) {
        const g = s.gpu?.find(g => g.name === state.selectedGpuLoad) || s.gpu?.[0];
        const minGPU = gpuMember(minimum?.gpu, g);
        const maxGPU = gpuMember(maximum?.gpu, g);
        if (wantsChart(state.charts.gpuload)) {
            const show = !!g && (g.load_pct > 0 || g.power_w > 0);
            setChartHidden('card-gpu-load', !show);
            if (show) {
                push(state.charts.gpuload.data.datasets[0], g.load_pct || 0, minGPU?.load_pct, maxGPU?.load_pct);
                push(state.charts.gpuload.data.datasets[1], g.power_w || 0, minGPU?.power_w, maxGPU?.power_w);
            }
        }
        if (wantsChart(state.charts.vram)) {
            const show = !!g && g.vram_total > 0 && g.vram_used > 0;
            setChartHidden('card-vram', !show);
            if (show) {
                push(state.charts.vram.data.datasets[0], g.vram_used, minGPU?.vram_used, maxGPU?.vram_used);
                state.charts.vram.options.scales.y.max = g.vram_total;
            }
        }
        if (wantsChart(state.charts.gputemp)) {
            const show = !!g && g.temp > 0;
            setChartHidden('card-gpu-temp', !show);
            if (show) {
                setChartHidden('thermals-title', false);
                setChartHidden('thermals-grid', false);
                push(state.charts.gputemp.data.datasets[0], g.temp, minGPU?.temp, maxGPU?.temp);
            }
        }
        if (state.focusMode) updateChartUI('focus-layout', applyStoredFocusMode);
    }

    // ---- Power Supply (batteries/UPS) — dynamic charts in system metrics grid ----
    if (!charts && s.psu && s.psu.length > 0) {
        for (const ps of s.psu) {
            // Only chart batteries and UPS, skip Mains adapters
            if (ps.type !== 'Battery' && ps.type !== 'UPS') continue;

            const psuKey = `psu_${ps.name}`;
            const minPSU = memberBy(minimum?.psu, 'name', ps.name);
            const maxPSU = memberBy(maximum?.psu, 'name', ps.name);
            if (!state.psuCharts) state.psuCharts = {};

            if (!state.psuCharts[psuKey]) {
                const grid = document.getElementById('charts-grid');
                if (grid) {
                    const wrapper = document.createElement('div');
                    wrapper.className = 'chart-card';
                    wrapper.id = `card-${psuKey}`;
                    wrapper.dataset.systemChart = '';
                    const header = document.createElement('div');
                    header.className = 'chart-header';
                    const h3 = document.createElement('h3');
                    h3.textContent = `${ps.type} \u2014 ${ps.name}`;
                    const span = document.createElement('span');
                    span.className = 'chart-subtitle';
                    span.id = `${psuKey}-subtitle`;
                    header.appendChild(h3);
                    header.appendChild(span);
                    const body = document.createElement('div');
                    body.className = 'chart-body';
                    const canvas = document.createElement('canvas');
                    canvas.id = `chart-${psuKey}`;
                    body.appendChild(canvas);
                    wrapper.appendChild(header);
                    wrapper.appendChild(body);
                    grid.appendChild(wrapper);
                    attachDynamicChartCardActions(wrapper);

                    if (state.focusSelecting) {
                        if (state.focusVisible?.includes(wrapper.id)) {
                            wrapper.classList.add('focus-selected');
                        }
                        wrapper._focusClick = () => wrapper.classList.toggle('focus-selected');
                        wrapper.addEventListener('click', wrapper._focusClick);
                    }

                    state.psuCharts[psuKey] = createTimeSeriesChart(`chart-${psuKey}`, [
                        { label: 'Capacity %', borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
                        { label: 'Power W', borderColor: colors.orange, data: [], fill: false },
                    ], { beginAtZero: true, max: 100, ticks: { callback: v => formatMetricNumber(v) + '%' } });

                    // Add second y-axis for power
                    if (state.psuCharts[psuKey]) {
                        state.psuCharts[psuKey].data.datasets[1].yAxisID = 'y1';
                        state.psuCharts[psuKey].options.scales.y1 = {
                            position: 'right',
                            beginAtZero: true,
                            grid: { display: false },
                            ticks: { callback: v => v.toFixed(1) + ' W' },
                        };
                        queueChartUpdate(state.psuCharts[psuKey]);
                    }

                    // A stored focus selection may be restored before telemetry
                    // creates this card, so re-apply it once the card exists.
                    if (state.focusMode && !state.focusSelecting) {
                        updateChartUI('focus-layout', applyStoredFocusMode);
                    }
                }
            }

            const chart = state.psuCharts[psuKey];
            if (chart) {
                push(chart.data.datasets[0], ps.capacity || 0, minPSU?.capacity, maxPSU?.capacity);
                push(chart.data.datasets[1], ps.power_w || 0, minPSU?.power_w, maxPSU?.power_w);
                if (!state.loadingHistory) queueChartUpdate(chart);
            }

            setChartSubtitle(`${psuKey}-subtitle`, () => {
                const parts = [`${formatMetricNumber(ps.capacity)}%`];
                if (ps.status) parts.push(ps.status);
                if (ps.power_w > 0) parts.push(`${ps.power_w.toFixed(1)} W`);
                if (ps.voltage_v > 0) parts.push(`${ps.voltage_v.toFixed(2)} V`);
                return parts.join('  ');
            });
        }
    }

    // ---- Applications (all charts created dynamically) ----
    let appsVisible = false;
    const seenCustom = new Set();
    const colorList = [colors.blue, colors.green, colors.orange, colors.purple, colors.cyan, colors.red, colors.yellow, colors.pink, colors.teal, colors.lime];

    // Nginx — create charts on first data, push data, update subtitles
    if (!charts && s.apps?.nginx) {
        const n = s.apps.nginx;
        const minN = minimum?.apps?.nginx;
        const maxN = maximum?.apps?.nginx;
        appsVisible = true;

        if (!state.charts.nginxConn) {
            createAppChartCard('card-nginx-connections', 'chart-nginx-connections', 'nginx-conn-subtitle', 'Nginx \u2014 Connections', APP_ORDER_NGINX);
            state.charts.nginxConn = createTimeSeriesChart('chart-nginx-connections', [
                { label: 'Active Connections', borderColor: colors.blue, backgroundColor: colors.blueAlpha, fill: true, data: [] },
            ]);
        }
        if (state.charts.nginxConn) {
            push(state.charts.nginxConn.data.datasets[0], n.active_conn, minN?.active_conn, maxN?.active_conn);
            setChartSubtitle('nginx-conn-subtitle', () => `Active: ${n.active_conn}`);
        }

        if (!state.charts.nginxReqs) {
            createAppChartCard('card-nginx-requests', 'chart-nginx-requests', 'nginx-reqs-subtitle', 'Nginx \u2014 Requests', APP_ORDER_NGINX + 1);
            state.charts.nginxReqs = createTimeSeriesChart('chart-nginx-requests', [
                { label: 'Accepts/s', borderColor: colors.green, data: [], fill: false },
                { label: 'Handled/s', borderColor: colors.cyan, data: [], fill: false },
                { label: 'Requests/s', borderColor: colors.blue, backgroundColor: colors.blueAlpha, fill: true, data: [] },
            ]);
        }
        if (state.charts.nginxReqs) {
            pushFields(state.charts.nginxReqs, n, minN, maxN, ['accepts_ps', 'handled_ps', 'requests_ps']);
            setChartSubtitle('nginx-reqs-subtitle', () => `Req/s: ${n.requests_ps?.toFixed(1) || '0'}`);
        }

        if (!state.charts.nginxRw) {
            createAppChartCard('card-nginx-rw', 'chart-nginx-rw', 'nginx-rw-subtitle', 'Nginx \u2014 Workers', APP_ORDER_NGINX + 2);
            state.charts.nginxRw = createTimeSeriesChart('chart-nginx-rw', [
                { label: 'Reading', borderColor: colors.green, data: [], fill: false },
                { label: 'Writing', borderColor: colors.orange, data: [], fill: false },
                { label: 'Waiting', borderColor: colors.yellow, data: [], fill: false },
            ]);
        }
        if (state.charts.nginxRw) {
            pushFields(state.charts.nginxRw, n, minN, maxN, ['reading', 'writing', 'waiting']);
            setChartSubtitle('nginx-rw-subtitle', () => `R: ${n.reading}  W: ${n.writing}  Wait: ${n.waiting}`);
        }
    }

    // Apache2 — create charts on first data, push data, update subtitles
    if (!charts && s.apps?.apache2) {
        const a = s.apps.apache2;
        const minA = minimum?.apps?.apache2;
        const maxA = maximum?.apps?.apache2;
        appsVisible = true;

        if (!state.charts.apache2Workers) {
            createAppChartCard('card-apache2-workers', 'chart-apache2-workers', 'apache2-workers-subtitle', 'Apache2 \u2014 Workers', APP_ORDER_APACHE2);
            state.charts.apache2Workers = createTimeSeriesChart('chart-apache2-workers', [
                { label: 'Busy', borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true, data: [] },
                { label: 'Idle', borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
            ]);
        }
        if (state.charts.apache2Workers) {
            pushFields(state.charts.apache2Workers, a, minA, maxA, ['busy_workers', 'idle_workers']);
            setChartSubtitle('apache2-workers-subtitle', () => `Busy: ${a.busy_workers}  Idle: ${a.idle_workers}`);
        }

        if (!state.charts.apache2Tput) {
            createAppChartCard('card-apache2-throughput', 'chart-apache2-throughput', 'apache2-tput-subtitle', 'Apache2 \u2014 Throughput', APP_ORDER_APACHE2 + 1);
            state.charts.apache2Tput = createTimeSeriesChart('chart-apache2-throughput', [
                { label: 'Accesses/s', borderColor: colors.green, data: [], fill: false },
                { label: 'Req/s', borderColor: colors.blue, backgroundColor: colors.blueAlpha, fill: true, data: [] },
                { label: 'kB/s', borderColor: colors.purple, data: [], fill: false },
            ]);
        }
        if (state.charts.apache2Tput) {
            pushFields(state.charts.apache2Tput, a, minA, maxA, ['accesses_ps', 'req_per_sec', 'kbytes_ps']);
            setChartSubtitle('apache2-tput-subtitle', () => `Req/s: ${a.req_per_sec?.toFixed(1) || '0'}  kB/s: ${a.kbytes_ps?.toFixed(1) || '0'}`);
        }

        if (!state.charts.apache2States) {
            const cl = colorList;
            createAppChartCard('card-apache2-states', 'chart-apache2-states', 'apache2-states-subtitle', 'Apache2 \u2014 Worker States', APP_ORDER_APACHE2 + 2);
            state.charts.apache2States = createTimeSeriesChart('chart-apache2-states', [
                { label: 'Waiting',     borderColor: cl[0], data: [], fill: false },
                { label: 'Reading',     borderColor: cl[1], data: [], fill: false },
                { label: 'Sending',     borderColor: cl[2], data: [], fill: false },
                { label: 'Keepalive',   borderColor: cl[3], data: [], fill: false },
                { label: 'Starting',    borderColor: cl[4], data: [], fill: false },
                { label: 'DNS',         borderColor: cl[5], data: [], fill: false },
                { label: 'Closing',     borderColor: cl[6], data: [], fill: false },
                { label: 'Logging',     borderColor: cl[7], data: [], fill: false },
                { label: 'Graceful',    borderColor: cl[8], data: [], fill: false },
                { label: 'IdleCleanup', borderColor: cl[9], data: [], fill: false },
                { label: 'OpenSlots',   borderColor: cl[0], borderDash: [3, 2], data: [], fill: false },
            ]);
        }
        if (state.charts.apache2States) {
            pushFields(
                state.charts.apache2States,
                a,
                minA,
                maxA,
                ['waiting', 'reading', 'sending', 'keepalive', 'starting', 'dns', 'closing', 'logging', 'graceful', 'idle_cleanup', 'open_slots'],
            );
            setChartSubtitle('apache2-states-subtitle', () => `Busy: ${a.busy_workers}  Idle: ${a.idle_workers}  Slots: ${a.open_slots}`);
        }
    }

    // MySQL — create charts on first data
    if (!charts && s.apps?.mysql) {
        const m = s.apps.mysql;
        const minM = minimum?.apps?.mysql;
        const maxM = maximum?.apps?.mysql;
        appsVisible = true;

        // 1. Connection States
        if (!state.charts.mysqlConn) {
            createAppChartCard('card-mysql-connections', 'chart-mysql-connections', 'mysql-conn-subtitle', 'MySQL \u2014 Connection States', APP_ORDER_MYSQL);
            state.charts.mysqlConn = createTimeSeriesChart('chart-mysql-connections', [
                { label: 'Connected',      borderColor: colors.blue,   backgroundColor: colors.blueAlpha,   fill: true,  data: [] },
                { label: 'Running',         borderColor: colors.green,  backgroundColor: colors.greenAlpha,  fill: true,  data: [] },
                { label: 'Cached',          borderColor: colors.yellow, backgroundColor: colors.yellowAlpha, fill: true,  data: [] },
                { label: 'Max Connections', borderColor: colors.purple, data: [], fill: false, borderDash: [4, 2] },
            ]);
        }
        if (state.charts.mysqlConn) {
            pushFields(state.charts.mysqlConn, m, minM, maxM, ['threads_connected', 'threads_running', 'threads_cached', 'max_conns']);
            setChartSubtitle('mysql-conn-subtitle', () => `Connected: ${m.threads_connected}  Running: ${m.threads_running}  Cached: ${m.threads_cached}  Max: ${m.max_conns}`);
        }

        // 2. Queries per Second
        if (!state.charts.mysqlQPS) {
            createAppChartCard('card-mysql-queries', 'chart-mysql-queries', 'mysql-qps-subtitle', 'MySQL \u2014 Queries/s', APP_ORDER_MYSQL + 1);
            state.charts.mysqlQPS = createTimeSeriesChart('chart-mysql-queries', [
                { label: 'Queries/s',  borderColor: colors.blue,   backgroundColor: colors.blueAlpha, fill: true, data: [] },
                { label: 'Select/s',   borderColor: colors.green,  data: [], fill: false },
                { label: 'Insert/s',   borderColor: colors.cyan,   data: [], fill: false },
                { label: 'Update/s',   borderColor: colors.orange, data: [], fill: false },
                { label: 'Delete/s',   borderColor: colors.red,    data: [], fill: false },
            ]);
        }
        if (state.charts.mysqlQPS) {
            pushFields(state.charts.mysqlQPS, m, minM, maxM, ['queries_ps', 'select_ps', 'insert_ps', 'update_ps', 'delete_ps']);
            setChartSubtitle('mysql-qps-subtitle', () => `QPS: ${(m.queries_ps || 0).toFixed(1)}  Sel: ${(m.select_ps || 0).toFixed(1)}  Ins: ${(m.insert_ps || 0).toFixed(1)}`);
        }

        // 3. Slow Queries
        if (!state.charts.mysqlSlow) {
            createAppChartCard('card-mysql-slow', 'chart-mysql-slow', 'mysql-slow-subtitle', 'MySQL \u2014 Slow Queries/s', APP_ORDER_MYSQL + 2);
            state.charts.mysqlSlow = createTimeSeriesChart('chart-mysql-slow', [
                { label: 'Slow Queries/s', borderColor: colors.red, backgroundColor: colors.redAlpha, fill: true, data: [] },
            ]);
        }
        if (state.charts.mysqlSlow) {
            push(state.charts.mysqlSlow.data.datasets[0], m.slow_queries_ps, minM?.slow_queries_ps, maxM?.slow_queries_ps);
            setChartSubtitle('mysql-slow-subtitle', () => `Slow: ${(m.slow_queries_ps || 0).toFixed(2)}`);
        }

        // 4. InnoDB Buffer Pool
        if (!state.charts.mysqlInnoDB) {
            createAppChartCard('card-mysql-innodb', 'chart-mysql-innodb', 'mysql-innodb-subtitle', 'MySQL \u2014 InnoDB Buffer Pool', APP_ORDER_MYSQL + 3);
            state.charts.mysqlInnoDB = createTimeSeriesChart('chart-mysql-innodb', [
                { label: 'Hit %',      borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
                { label: 'Reads/s',    borderColor: colors.orange, data: [], fill: false },
            ], { ticks: { callback: v => v.toFixed(1) + '%' } });
        }
        if (state.charts.mysqlInnoDB) {
            pushFields(state.charts.mysqlInnoDB, m, minM, maxM, ['innodb_buffer_pool_hit_pct', 'innodb_bp_reads_ps']);
            setChartSubtitle('mysql-innodb-subtitle', () => `Hit: ${(m.innodb_buffer_pool_hit_pct || 0).toFixed(1)}%  Reads/s: ${(m.innodb_bp_reads_ps || 0).toFixed(0)}`);
        }

        // 5. Lock Waits
        if (!state.charts.mysqlLocks) {
            createAppChartCard('card-mysql-locks', 'chart-mysql-locks', 'mysql-locks-subtitle', 'MySQL \u2014 Lock Waits/s', APP_ORDER_MYSQL + 4);
            state.charts.mysqlLocks = createTimeSeriesChart('chart-mysql-locks', [
                { label: 'Table Lock Waits/s', borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true,  data: [] },
                { label: 'Row Lock Waits/s',   borderColor: colors.red,    data: [], fill: false },
            ]);
        }
        if (state.charts.mysqlLocks) {
            pushFields(state.charts.mysqlLocks, m, minM, maxM, ['table_locks_waited_ps', 'row_lock_waits_ps']);
            setChartSubtitle('mysql-locks-subtitle', () => `Table: ${(m.table_locks_waited_ps || 0).toFixed(2)}  Row: ${(m.row_lock_waits_ps || 0).toFixed(2)}`);
        }

        // 6. Replication — only render the card when the server actually
        // participates in replication: the field is always present in JSON
        // for v2+ records, so a presence check would render an empty card
        // on every standalone server. Detect "is a replica" via either
        // thread being running OR a non-sentinel seconds-behind value, and
        // "is a primary with replicas" via replica_count > 0.
        const mIsReplica = m.replica_io_running || m.replica_sql_running ||
            (typeof m.replica_seconds_behind === 'number' && m.replica_seconds_behind >= 0);
        const mHasReplicas = (m.replica_count || 0) > 0;
        if (mIsReplica || mHasReplicas) {
            if (!state.charts.mysqlRepl) {
                createAppChartCard('card-mysql-replication', 'chart-mysql-replication', 'mysql-repl-subtitle', 'MySQL — Replication', APP_ORDER_MYSQL + 5);
                state.charts.mysqlRepl = createTimeSeriesChart('chart-mysql-replication', [
                    { label: 'Seconds Behind',     borderColor: colors.red,    backgroundColor: colors.redAlpha,    fill: true,  data: [] },
                    { label: 'Replicas Connected', borderColor: colors.blue,   data: [], fill: false },
                ]);
            }
            if (state.charts.mysqlRepl) {
                const secs = (typeof m.replica_seconds_behind === 'number' && m.replica_seconds_behind >= 0) ? m.replica_seconds_behind : null;
                push(state.charts.mysqlRepl.data.datasets[0], secs, minM?.replica_seconds_behind, maxM?.replica_seconds_behind);
                push(state.charts.mysqlRepl.data.datasets[1], m.replica_count, minM?.replica_count, maxM?.replica_count);
                setChartSubtitle('mysql-repl-subtitle', () => {
                    const io  = m.replica_io_running  ? 'running' : 'stopped';
                    const sql = m.replica_sql_running ? 'running' : 'stopped';
                    const lag = secs === null ? 'n/a' : `${secs}s`;
                    // Surface a non-zero errno or a non-waiting IO state
                    // inline so an operator alerted by the IO/SQL flip can
                    // see *why* without opening a SQL client.
                    const errno = (m.replica_last_io_errno || m.replica_last_sql_errno) || 0;
                    let extra = '';
                    if (errno) extra = `  Err: ${errno}`;
                    else if (m.replica_io_state && !/waiting for (master|source) to send/i.test(m.replica_io_state)) {
                        extra = `  State: ${m.replica_io_state}`;
                    }
                    return `IO: ${io}  SQL: ${sql}  Lag: ${lag}  Replicas: ${m.replica_count || 0}${extra}`;
                });
            }
        }
    }

    // Containers — one multi-series chart per metric type with app filter.
    // Always call the container pipeline so absent ticks stay time-aligned and
    // cards hide when no containers remain (even if Nginx keeps the section open).
    if (!charts && s.apps?.containers?.length > 0) {
        appsVisible = true;
            addContainerSample(
                s.apps.containers,
                ts,
                createAppChartCard,
                minimum?.apps?.containers,
                maximum?.apps?.containers,
                hasEnvelope,
            );
    } else if (!charts) {
        markContainersAbsent(ts);
    }

    // PostgreSQL — create charts on first data
    if (!charts && s.apps?.postgres) {
        const pg = s.apps.postgres;
        const minPG = minimum?.apps?.postgres;
        const maxPG = maximum?.apps?.postgres;
        appsVisible = true;

        // 1. Connection States (stacked area)
        if (!state.charts.pgConnStates) {
            createAppChartCard('card-pg-connections', 'chart-pg-connections', 'pg-conn-subtitle', 'PostgreSQL \u2014 Connection States', APP_ORDER_POSTGRES);
            state.charts.pgConnStates = createTimeSeriesChart('chart-pg-connections', [
                { label: 'Active',          borderColor: colors.blue,   backgroundColor: colors.blueAlpha,   fill: true,  data: [] },
                { label: 'Idle',            borderColor: colors.green,  backgroundColor: colors.greenAlpha,  fill: true,  data: [] },
                { label: 'Idle in Tx',      borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true,  data: [] },
                { label: 'Waiting',         borderColor: colors.red,    backgroundColor: colors.redAlpha,    fill: true,  data: [] },
                { label: 'Max Connections', borderColor: colors.purple, data: [], fill: false, borderDash: [4, 2] },
            ]);
        }
        if (state.charts.pgConnStates) {
            pushFields(state.charts.pgConnStates, pg, minPG, maxPG, ['active_conns', 'idle_conns', 'idle_in_tx_conns', 'waiting_conns', 'max_conns']);
            setChartSubtitle('pg-conn-subtitle', () => `Active: ${pg.active_conns}  Idle: ${pg.idle_conns}  IdleTx: ${pg.idle_in_tx_conns}  Wait: ${pg.waiting_conns}`);
        }

        // 2. Transactions per Second
        if (!state.charts.pgTPS) {
            createAppChartCard('card-pg-tps', 'chart-pg-tps', 'pg-tps-subtitle', 'PostgreSQL \u2014 Transactions/s', APP_ORDER_POSTGRES + 1);
            state.charts.pgTPS = createTimeSeriesChart('chart-pg-tps', [
                { label: 'Commits/s',   borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
                { label: 'Rollbacks/s', borderColor: colors.red,   data: [], fill: false },
            ]);
        }
        if (state.charts.pgTPS) {
            pushFields(state.charts.pgTPS, pg, minPG, maxPG, ['tx_commit_ps', 'tx_rollback_ps']);
            setChartSubtitle('pg-tps-subtitle', () => `Commits/s: ${(pg.tx_commit_ps || 0).toFixed(1)}  Rollbacks/s: ${(pg.tx_rollback_ps || 0).toFixed(1)}`);
        }

        // 3. Lock Waits & Deadlocks
        if (!state.charts.pgLocks) {
            createAppChartCard('card-pg-locks', 'chart-pg-locks', 'pg-locks-subtitle', 'PostgreSQL \u2014 Lock Waits & Deadlocks', APP_ORDER_POSTGRES + 2);
            state.charts.pgLocks = createTimeSeriesChart('chart-pg-locks', [
                { label: 'Lock Waits',   borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true, data: [] },
                { label: 'Deadlocks/s',  borderColor: colors.red,    data: [], fill: false },
            ]);
        }
        if (state.charts.pgLocks) {
            pushFields(state.charts.pgLocks, pg, minPG, maxPG, ['waiting_conns', 'deadlocks_ps']);
            setChartSubtitle('pg-locks-subtitle', () => `Lock Waits: ${pg.waiting_conns}  Deadlocks/s: ${(pg.deadlocks_ps || 0).toFixed(2)}`);
        }

        // 4. Row/Tuple Activity
        if (!state.charts.pgTuples) {
            createAppChartCard('card-pg-tuples', 'chart-pg-tuples', 'pg-tuples-subtitle', 'PostgreSQL \u2014 Row Activity', APP_ORDER_POSTGRES + 3);
            state.charts.pgTuples = createTimeSeriesChart('chart-pg-tuples', [
                { label: 'Fetched/s',  borderColor: colors.blue,   data: [], fill: false },
                { label: 'Returned/s', borderColor: colors.cyan,   data: [], fill: false },
                { label: 'Inserted/s', borderColor: colors.green,  data: [], fill: false },
                { label: 'Updated/s',  borderColor: colors.yellow, data: [], fill: false },
                { label: 'Deleted/s',  borderColor: colors.red,    data: [], fill: false },
            ]);
        }
        if (state.charts.pgTuples) {
            pushFields(state.charts.pgTuples, pg, minPG, maxPG, ['tup_fetched_ps', 'tup_returned_ps', 'tup_inserted_ps', 'tup_updated_ps', 'tup_deleted_ps']);
            setChartSubtitle('pg-tuples-subtitle', () => `Fetched/s: ${(pg.tup_fetched_ps || 0).toFixed(1)}  Ins: ${(pg.tup_inserted_ps || 0).toFixed(1)}  Upd: ${(pg.tup_updated_ps || 0).toFixed(1)}  Del: ${(pg.tup_deleted_ps || 0).toFixed(1)}`);
        }

        // 5. Disk I/O vs Memory (blocks)
        if (!state.charts.pgIO) {
            createAppChartCard('card-pg-io', 'chart-pg-io', 'pg-io-subtitle', 'PostgreSQL \u2014 Disk I/O vs Cache', APP_ORDER_POSTGRES + 4);
            state.charts.pgIO = createTimeSeriesChart('chart-pg-io', [
                { label: 'Blks Hit/s',  borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
                { label: 'Blks Read/s', borderColor: colors.orange, data: [], fill: false },
            ]);
        }
        if (state.charts.pgIO) {
            pushFields(state.charts.pgIO, pg, minPG, maxPG, ['blks_hit_ps', 'blks_read_ps']);
            setChartSubtitle('pg-io-subtitle', () => `Hit/s: ${(pg.blks_hit_ps || 0).toFixed(0)}  Read/s: ${(pg.blks_read_ps || 0).toFixed(0)}`);
        }

        // 6. Cache Hit Ratio
        if (!state.charts.pgCacheHit) {
            createAppChartCard('card-pg-cache-hit', 'chart-pg-cache-hit', 'pg-cache-subtitle', 'PostgreSQL \u2014 Cache Hit Ratio', APP_ORDER_POSTGRES + 5);
            state.charts.pgCacheHit = createTimeSeriesChart('chart-pg-cache-hit', [
                { label: 'Cache Hit %', borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
            ], { min: 0, max: 100, ticks: { callback: v => v.toFixed(1) + '%' } });
        }
        if (state.charts.pgCacheHit) {
            push(state.charts.pgCacheHit.data.datasets[0], pg.blks_hit_pct, minPG?.blks_hit_pct, maxPG?.blks_hit_pct);
            setChartSubtitle('pg-cache-subtitle', () => `Hit: ${(pg.blks_hit_pct || 0).toFixed(1)}%`);
        }

        // 7. Table Health
        if (!state.charts.pgTableHealth) {
            createAppChartCard('card-pg-table-health', 'chart-pg-table-health', 'pg-table-subtitle', 'PostgreSQL \u2014 Table Health', APP_ORDER_POSTGRES + 6);
            state.charts.pgTableHealth = createTimeSeriesChart('chart-pg-table-health', [
                { label: 'Dead Tuples', borderColor: colors.red,   backgroundColor: colors.redAlpha,   fill: true, data: [] },
                { label: 'Live Tuples', borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
            ]);
        }
        if (state.charts.pgTableHealth) {
            pushFields(state.charts.pgTableHealth, pg, minPG, maxPG, ['dead_tuples', 'live_tuples']);
            setChartSubtitle('pg-table-subtitle', () => `Dead: ${(pg.dead_tuples || 0).toLocaleString()}  Live: ${(pg.live_tuples || 0).toLocaleString()}  Vacuums: ${pg.autovacuum_count || 0}`);
        }

        // 8. Background Writer
        if (!state.charts.pgBgwriter) {
            createAppChartCard('card-pg-bgwriter', 'chart-pg-bgwriter', 'pg-bgwriter-subtitle', 'PostgreSQL \u2014 Background Writer', APP_ORDER_POSTGRES + 7);
            state.charts.pgBgwriter = createTimeSeriesChart('chart-pg-bgwriter', [
                { label: 'Checkpoint Bufs/s', borderColor: colors.blue,   backgroundColor: colors.blueAlpha,   fill: true, data: [] },
                { label: 'Backend Bufs/s',    borderColor: colors.orange, data: [], fill: false },
            ]);
        }
        if (state.charts.pgBgwriter) {
            pushFields(state.charts.pgBgwriter, pg, minPG, maxPG, ['buf_checkpoint_ps', 'buf_backend_ps']);
            setChartSubtitle('pg-bgwriter-subtitle', () => `Checkpoint: ${(pg.buf_checkpoint_ps || 0).toFixed(1)}/s  Backend: ${(pg.buf_backend_ps || 0).toFixed(1)}/s`);
        }

        // 9. Replication — only render when the server actually participates
        // in replication (standby OR primary with connected replicas). The
        // is_in_recovery field is always present in v3+ JSON, so a presence
        // check would render an empty card on every standalone server.
        if (pg.is_in_recovery || (pg.replica_count || 0) > 0) {
            if (!state.charts.pgRepl) {
                createAppChartCard('card-pg-replication', 'chart-pg-replication', 'pg-repl-subtitle', 'PostgreSQL — Replication', APP_ORDER_POSTGRES + 8);
                state.charts.pgRepl = createTimeSeriesChart('chart-pg-replication', [
                    { label: 'Lag Seconds',        borderColor: colors.red,  backgroundColor: colors.redAlpha, fill: true,  data: [] },
                    { label: 'Replicas Connected', borderColor: colors.blue, data: [], fill: false },
                ]);
            }
            if (state.charts.pgRepl) {
                pushFields(state.charts.pgRepl, pg, minPG, maxPG, ['repl_lag_seconds', 'replica_count']);
                setChartSubtitle('pg-repl-subtitle', () => {
                    if (pg.is_in_recovery) {
                        return `Standby  Lag: ${(pg.repl_lag_seconds || 0).toFixed(2)}s / ${formatBytesShort(pg.repl_lag_bytes || 0)}`;
                    }
                    return `Primary  Replicas: ${pg.replica_count || 0}`;
                });
            }
        }
    }

    // Custom metrics (dynamic charts per group, appended directly to grid)
    if (!charts && s.apps?.custom) {
        for (const [group, metrics] of Object.entries(s.apps.custom)) {
            appsVisible = true;
            if (!state.customCharts[group]) {
                const cfgList = state.customMetricsConfig?.[group] || [];
                const unit = cfgList.length > 0 ? cfgList[0].unit : '';
                let maxVal = undefined;
                if (cfgList.length > 0) {
                    const m = Math.max(...cfgList.map(c => c.max || 0));
                    if (m > 0 && m !== -Infinity) maxVal = m;
                }

                const datasets = cfgList.map((cfg, i) => ({
                    label: cfg.name + (unit ? ` (${unit})` : ''),
                    borderColor: colorList[i % colorList.length],
                    data: [],
                    fill: false,
                    pointRadius: 0,
                    borderWidth: 1.5,
                    tension: 0,
                }));

                const title = group.replace(/_/g, ' ').replace(/\b\w/g, c => c.toUpperCase());
                createAppChartCard(`card-custom-${group}`, `chart-custom-${group}`, `custom-${group}-subtitle`, title, APP_ORDER_CUSTOM);

                const canvas = document.getElementById(`chart-custom-${group}`);
                const ctx = canvas?.getContext('2d');
                if (ctx) {
                    const yConfig = { beginAtZero: true };
                    if (maxVal) yConfig.max = maxVal;
                    if (unit) yConfig.title = { display: true, text: unit };

                    state.customCharts[group] = {
                        chart: createTimeSeriesChart(canvas.id, datasets, yConfig),
                        names: cfgList.map(c => c.name),
                    };
                }
            }

            const entry = state.customCharts[group];
            if (entry?.chart) {
                const valMap = {};
                for (const m of metrics) {
                    valMap[m.name] = m.value;
                }
                for (let i = 0; i < entry.names.length; i++) {
                    const v = valMap[entry.names[i]] ?? null;
                    const minMetric = memberBy(minimum?.apps?.custom?.[group], 'name', entry.names[i]);
                    const maxMetric = memberBy(maximum?.apps?.custom?.[group], 'name', entry.names[i]);
                    push(entry.chart.data.datasets[i], v, minMetric?.value, maxMetric?.value);
                }
                if (!state.loadingHistory) queueChartUpdate(entry.chart);

                setChartSubtitle(`custom-${group}-subtitle`, () =>
                    metrics.map(m => `${m.name}: ${formatMetricNumber(m.value)}`).join('  '));
            }
            seenCustom.add(group);
        }
    }
    // Hide custom cards not in this sample
    if (!charts) updateChartUI('custom-cards', () => {
        Object.keys(state.customCharts || {}).forEach(k => {
            const el = document.getElementById(`card-custom-${k}`);
            const chart = state.customCharts[k]?.chart;
            const hasHistory = chart?.data?.datasets?.some(hasEnvelopeData);
            if (seenCustom.has(k) || hasHistory) {
                el?.classList.remove('hidden');
            } else {
                el?.classList.add('hidden');
            }
        });
    });

    // Optional sections must contribute an explicit null tick when absent.
    // Otherwise Chart.js connects the last pre-outage value directly to the
    // first recovered value and visually erases the outage.
    const appendMissing = chart => {
        if (charts && !charts.has(chart)) return;
        chart?.data?.datasets?.forEach(dataset => {
            if (!touchedDatasets.has(dataset) && dataset.data?.length > 0) {
                appendEnvelopeGap(dataset, ts);
            }
        });
    };
    Object.values(state.charts).forEach(appendMissing);
    Object.values(state.psuCharts || {}).forEach(appendMissing);
    Object.values(state.customCharts || {}).forEach(entry => appendMissing(entry?.chart));

    // Show/hide applications section
    if (!charts) updateChartUI('applications-section', () => {
        const titleEl = document.getElementById('applications-title');
        const headerEl = document.getElementById('applications-header');
        const gridEl = document.getElementById('applications-grid');
        const hasVisibleHistory = !!gridEl?.querySelector('.chart-card:not(.hidden)');
        if (appsVisible || hasVisibleHistory) {
            titleEl?.classList.remove('hidden');
            headerEl?.classList.remove('hidden');
            gridEl?.classList.remove('hidden');
        } else {
            titleEl?.classList.add('hidden');
            headerEl?.classList.add('hidden');
            gridEl?.classList.add('hidden');
        }
    });

    // Feed split charts
    if (!charts) addSampleToSplitCharts(s, minimum, maximum, ts, hasEnvelope);
}

// Mark every chart dirty. The chart controller coalesces calls into one
// animation frame and leaves off-screen charts dirty until they enter the
// viewport.
export function updateAllCharts() {
    setChartTimeRange();
    if (typeof updateChartLabels === 'function') {
        updateChartLabels();
    }
    queueAllChartUpdates();
}

// Replay the active buffer. Device selectors pass chart keys so unrelated
// datasets, shared gap metadata, and chart updates remain untouched.
export function redrawChartsFromBuffer(chartKeys = null) {
    const charts = chartKeys ? new Set(chartKeys.map(key => state.charts[key]).filter(Boolean)) : null;
    clearAllChartData(charts);
    renderHistoryItems(state.dataBuffer, charts);
    if (charts) charts.forEach(queueChartUpdate);
    else updateAllCharts();

    // Also update subtitles and gauges with the latest buffer item
    if (state.lastSample) {
        updateSubtitles(state.lastSample);
        const selectors = state.timeRange === null
            ? historyItemSample(latestHistoryItem(state.dataBuffer)) : state.lastSample;
        if (selectors) updateSelectors(selectors);
    }
}

export function trimChartsToTimeRange() {
    const viewEnd = state.historyViewEnd ?? Date.now();
    const cutoffMs = state.timeRange === null ? null : viewEnd - state.timeRange * 1000;

    const trimChart = (chart) => {
        if (!chart || !chart.data?.datasets) return;
        chart.data.datasets.forEach(ds => {
            if (!Array.isArray(ds.data) || ds.data.length === 0) return;
            let i = 0;
            if (cutoffMs !== null) {
                while (i < ds.data.length && ds.data[i].x != null && ds.data[i].x < cutoffMs) i++;
            }
            if (i > 0) trimEnvelopeData(ds, i);
        });
    };

    Object.values(state.charts).forEach(trimChart);
    Object.values(state.splitCharts).forEach(typeCharts => {
        Object.values(typeCharts).forEach(trimChart);
    });
    forEachAppChart(trimChart);

    if (cutoffMs !== null) {
        state.historyGaps = (state.historyGaps || []).filter(gap => gap.end > cutoffMs);
    }

    // Keep dataBuffer in sync with the displayed time window
    let trimmedBuffer = false;
    if (cutoffMs !== null) {
        const cutoffDate = new Date(cutoffMs);
        let bi = 0;
        while (bi < state.dataBuffer.length) {
            const item = state.dataBuffer[bi];
            const gapEnd = item?._gap ? Date.parse(item.gap_end) : NaN;
            if (Number.isFinite(gapEnd) && gapEnd > cutoffMs) break;
            if (new Date(historyItemTimestamp(item)) >= cutoffDate) break;
            bi++;
        }
        if (bi > 0) {
            state.dataBuffer.splice(0, bi);
            trimmedBuffer = true;
        }
    }
    if (trimmedBuffer) {
        rebuildHistoryPointContexts();
    }
}

export function clearAllChartData(charts = null) {
    if (!charts) state.historyGaps = [];
    const clearChart = (chart) => {
        if (!chart?.data?.datasets) return;
        chart.data.datasets.forEach(ds => {
            if (Array.isArray(ds.data)) clearEnvelopeData(ds);
        });
    };
    if (charts) {
        charts.forEach(clearChart);
        return;
    }
    Object.values(state.charts).forEach(clearChart);
    // Also clear split charts
    Object.values(state.splitCharts).forEach(typeCharts => {
        Object.values(typeCharts).forEach(clearChart);
    });
    // Also clear dynamic app charts
    forEachAppChart(clearChart);
}

// Debounce timer for zoom-triggered history fetches.
export let _zoomFetchTimer = null;

function cancelPendingZoomFetch() {
    clearTimeout(_zoomFetchTimer);
    _zoomFetchTimer = null;
}

function setHistoryStatus(status, response = null, error = null) {
    state.historyStatus = status;
    state.historyError = error ? (error.message || String(error)) : null;

    if (response && !Array.isArray(response)) {
        state.historyCoverage = {
            requestedFrom: response.requested_from ?? null,
            requestedTo: response.requested_to ?? null,
            actualFrom: response.actual_from ?? null,
            actualTo: response.actual_to ?? null,
            complete: typeof response.exact_complete === 'boolean'
                ? response.exact_complete
                : response.complete !== false,
            retentionComplete: response.complete !== false,
            tier: response.tier ?? null,
            resolution: response.resolution ?? null,
            sourceResolution: response.source_resolution ?? response.resolution ?? null,
            downsampled: response.downsampled === true || (
                !!response.resolution && !!response.source_resolution &&
                response.resolution !== response.source_resolution
            ),
        };
    }

    const statusEl = document.getElementById('sampling-info');
    if (statusEl) {
        statusEl.dataset.historyStatus = status;
        if (state.historyError) statusEl.dataset.historyError = state.historyError;
        else delete statusEl.dataset.historyError;
        renderSamplingInfo(status);
    }

    const announcement = document.getElementById('history-status-announcement');
    if (announcement) {
        const key = {
            loading: 'history_loading',
            failed: 'history_failed',
            empty: 'history_empty',
            partial: 'history_partial',
            complete: 'history_complete',
        }[status];
        announcement.textContent = key ? i18n.t(key) : '';
    }

    forEachRegisteredChart(chart => {
        chart.$kulaHistoryStatus = status;
        updateChartAccessibility(chart);
    });

    document.dispatchEvent(new CustomEvent('kula-history-status', {
        detail: {
            status,
            error: state.historyError,
            coverage: state.historyCoverage,
            generation: state.historyRequestGeneration,
        },
    }));
}

function historyPayloadSamples(response) {
    if (Array.isArray(response)) return response;
    if (response && Object.prototype.hasOwnProperty.call(response, 'samples')) {
        if (response.samples == null) return [];
        if (Array.isArray(response.samples)) return response.samples;
    }
    throw new Error('Invalid history response');
}

function historyPayloadStatus(response, samples) {
    if (samples.length === 0) return 'empty';
    if (!Array.isArray(response) && response.complete === false) return 'partial';
    if (!Array.isArray(response) && state.timeRange === null && response.exact_complete === false) {
        return 'partial';
    }
    return 'complete';
}

function renderHistoryItem(item, charts = null) {
    if (!item) return;
    if (item._gap) {
        addGapToCharts(item, charts);
        return;
    }
    const timestamp = historyItemContext(item)?.timestamp ?? new Date(historyItemTimestamp(item)).getTime();
    addSampleToCharts(item, timestamp, { charts });
}

function renderHistoryItems(items, charts = null) {
    batchChartUI(() => {
        for (const item of items) renderHistoryItem(item, charts);
    });
}

function latestHistoryItem(items) {
    for (let i = items.length - 1; i >= 0; i--) {
        if (!items[i]?._gap && historyItemSample(items[i])) return items[i];
    }
    return null;
}

function replaceHistoryBuffer(samples, resolution, response = null) {
    const processed = insertGapsInHistory(annotateHistoryItems(samples, response), resolution);
    if (processed.length > state.maxBufferSize) throw new Error('History response exceeds point budget');
    const latest = latestHistoryItem(processed);
    if (latest) updateSelectors(historyItemSample(latest));

    clearAllChartData();
    state.dataBuffer = processed;
    renderHistoryItems(processed);
    rebuildHistoryPointContexts();

    return latest;
}

function updateLatestHistorySample(item) {
    const sample = historyItemSample(item);
    if (!sample) return;

    state.lastHistoricalTs = new Date(historyItemTimestamp(item));
    // A background history response must not move the live gauges backwards.
    if (state.lastSample && new Date(historyItemTimestamp(state.lastSample)) > state.lastHistoricalTs) return;
    state.lastSample = sample;
    updateGauges(sample);
    updateHeader(sample);
    updateSubtitles(sample);
    evaluateAlerts(sample);
}

function requestHistory(fromDate, toDate, points, apply, {
    label = 'History',
    showSpinner = true,
    queueLive = true,
} = {}) {
    // A delayed zoom must not start a new request after this newer view and
    // supersede it through the latest-request-wins controller.
    cancelPendingZoomFetch();
    const query = new URLSearchParams({
        from: fromDate.toISOString(),
        to: toDate.toISOString(),
        points: String(points),
    });
    const sections = historySectionsForFocus(state.focusMode, state.focusVisible);
    if (sections) query.set('sections', sections.join(','));

    return historyRequests.fetchJSON(apiUrl(`/api/history?${query.toString()}`), {
        onStart: generation => {
            state.historyRequestGeneration = generation;
            state.loadingHistory = true;
            state.queueLiveDuringHistory = queueLive;
            setHistoryStatus('loading');
            const spinner = document.getElementById('loading-spinner');
            spinner?.classList.toggle('hidden', !showSpinner);
        },
        onApply: (response, { isCurrent }) => {
            const samples = historyPayloadSamples(response);
            if (!Array.isArray(response)) {
                updateSamplingInfo(
                    response.tier,
                    response.resolution,
                    response.complete,
                    response.valid_aggregations,
                    response.source_resolution,
                    response.downsampled,
                );
            } else {
                applyAggregationValidity(['data']);
            }
            if (samples.length > points) throw new Error('History response exceeds requested point budget');
            state.historyPointLimit = points;
            apply(response, samples);
            if (!isCurrent()) return;
            setHistoryStatus(historyPayloadStatus(response, samples), response);
        },
        onFailure: error => {
            console.error('%s history fetch error:', label, error);
            setHistoryStatus('failed', null, error);
        },
        onFinish: () => {
            state.loadingHistory = false;
            state.queueLiveDuringHistory = false;
            document.getElementById('loading-spinner')?.classList.add('hidden');
            drainLiveQueue();
        },
    });
}

function supersedeHistoryRequest() {
    cancelPendingZoomFetch();
    state.historyRequestGeneration = historyRequests.supersede();
    state.loadingHistory = false;
    state.queueLiveDuringHistory = false;
    document.getElementById('loading-spinner')?.classList.add('hidden');
}

function releaseZoomPause() {
    if (!state.pausedZoom) return;
    state.pausedZoom = false;
    document.dispatchEvent(new Event('kula-sync-pause'));
}

function settleLocalHistoryView(fromDate, toDate, visible) {
    const previous = state.historyCoverage;
    const sourceResolution = previous?.sourceResolution || state.currentSourceResolution || state.currentResolution;
    const priorFrom = Date.parse(previous?.actualFrom);
    const priorTo = Date.parse(previous?.actualTo);
    const fromMs = fromDate.getTime();
    const toMs = toDate.getTime();
    const actualFrom = Number.isFinite(priorFrom) ? new Date(Math.max(fromMs, priorFrom)).toISOString() : null;
    const actualTo = Number.isFinite(priorTo) ? new Date(Math.min(toMs, priorTo)).toISOString() : null;
    const complete = previous?.complete === true && actualFrom !== null && actualTo !== null &&
        Date.parse(actualFrom) <= fromMs && Date.parse(actualTo) >= toMs &&
        !visible.some(item => item?._gap);
    state.historyCoverage = {
        requestedFrom: fromDate.toISOString(),
        requestedTo: toDate.toISOString(),
        actualFrom,
        actualTo,
        complete,
        retentionComplete: previous?.retentionComplete === true,
        tier: state.currentTier,
        resolution: state.currentResolution,
        sourceResolution,
        downsampled: state.currentDownsampled,
    };
    setHistoryStatus(complete ? 'complete' : 'partial');
    drainLiveQueue();
}

export function cancelHistoryRequest() {
    supersedeHistoryRequest();
    state.historyViewEnd = null;
    state.historyCoverage = null;
    setHistoryStatus('idle');
}

// tryZoomFromBuffer attempts to satisfy a zoom request from the in-memory
// data buffer, avoiding a network round-trip when the buffer already covers
// the requested window. Returns true if the redraw succeeded.
export function tryZoomFromBuffer(fromDate, toDate) {
    if (state.currentTier !== 0 || state.currentDownsampled) return false;
    if (!state.dataBuffer || state.dataBuffer.length === 0) return false;

    const fromMs = fromDate.getTime();
    const toMs   = toDate.getTime();

    const actualFrom = Date.parse(state.historyCoverage?.actualFrom);
    const actualTo = Date.parse(state.historyCoverage?.actualTo);
    if (!Number.isFinite(actualFrom) || !Number.isFinite(actualTo) ||
        actualFrom > fromMs || actualTo < toMs) return false;

    // Determine the time span of the current buffer.
    const first = state.dataBuffer[0];
    const last  = state.dataBuffer[state.dataBuffer.length - 1];
    const bufStart = new Date(historyItemTimestamp(first)).getTime();
    const bufEnd   = new Date(historyItemTimestamp(last)).getTime();

    if (isNaN(bufStart) || isNaN(bufEnd)) return false;
    if (bufStart > fromMs || bufEnd < toMs) return false; // buffer doesn't cover window

    supersedeHistoryRequest();

    // Buffer covers the window — redraw directly from it.
    state.timeRange = null;
    state.customFrom = fromDate;
    state.customTo = toDate;

    clearAllChartData();
    const visible = state.dataBuffer.filter(item => {
        const t = new Date(historyItemTimestamp(item)).getTime();
        return !isNaN(t) && t >= fromMs && t <= toMs;
    });
    renderHistoryItems(visible);
    updateAllCharts();
    settleLocalHistoryView(fromDate, toDate, visible);
    return true;
}

export function syncZoom(detail) {
    const sourceChart = detail?.chart || detail;
    const complete = detail?.chart ? detail.complete === true : true;
    const xOptions = sourceChart?.options?.scales?.x;
    const requestedMin = Number(xOptions?.min ?? sourceChart?.scales?.x?.min);
    const requestedMax = Number(xOptions?.max ?? sourceChart?.scales?.x?.max);
    const minSpan = minimumZoomSpan(state.currentSourceResolution, state.collectionIntervalMs);
    let bounded = clampHistoryInterval(requestedMin, requestedMax, Date.now(), undefined, minSpan);
    if (!bounded) return;
    const previousMin = state.timeRange === null ? +state.customFrom
        : (state.historyViewEnd ?? Date.now()) - state.timeRange * 1000;
    const previousMax = state.timeRange === null ? +state.customTo : (state.historyViewEnd ?? Date.now());
    if (requestedMax - requestedMin < previousMax - previousMin &&
        requestedMin >= previousMin && requestedMax <= previousMax) {
        // Count retained observations, not gap markers or nominal intervals.
        // A finer response can allow another zoom, but this gesture must not
        // discard all but a handful of the points currently being displayed.
        const span = bounded.max - bounded.min;
        bounded.min = Math.max(previousMin, Math.min(bounded.min, previousMax - span));
        bounded.max = Math.min(previousMax, bounded.min + span);
        const timestamps = state.dataBuffer.filter(item => !item?._gap && historyItemSample(item))
            .map(item => Date.parse(historyItemTimestamp(item)))
            .filter(ts => ts >= previousMin && ts <= previousMax);
        bounded = fitZoomToObservations(bounded, timestamps) || { min: previousMin, max: previousMax };
    }
    const { min, max } = bounded;

    // The viewport changes on the first movement, before pointerup or the
    // wheel debounce. Supersede both pending responses and delayed zoom fetches
    // immediately so neither can restore a previous viewport mid-gesture.
    supersedeHistoryRequest();

    // Update the display to show the zoomed timeframe explicitly
    state.timeRange = null; // Exact historical view: live chart insertion stops.
    state.customFrom = new Date(min);
    state.customTo = new Date(max);
    document.querySelectorAll('.time-btn[data-range]').forEach(button => button.classList.remove('active'));
    document.getElementById('btn-custom-range')?.classList.add('active');
    const fmt = date => formatRangeTimestamp(date, state.timeZone, i18n.currentLang);
    const zone = state.timeZone === 'utc' ? i18n.t('time_zone_utc') : i18n.t('time_zone_local');
    document.getElementById('time-range-display').textContent =
        `${fmt(state.customFrom)} \u2192 ${fmt(state.customTo)} · ${zone} (${i18n.t('zoomed')})`;

    const windowSec = (max - min) / 1000;
    const minUnit = windowSec >= 259200 ? 'day' : false; // 3 days

    forEachRegisteredChart(chart => {
        if (!chart?.options?.scales?.x) return;
        if (minUnit) {
            chart.options.scales.x.time.minUnit = minUnit;
        } else {
            delete chart.options.scales.x.time.minUnit;
        }
        chart.options.scales.x.min = min;
        chart.options.scales.x.max = max;
    });
    queueAllChartUpdates();

    // Continuous drag/pinch/wheel events only synchronize rendering. One
    // completed gesture owns the request, URL update, and navigation entry.
    if (!complete) return;
    document.dispatchEvent(new CustomEvent('kula-viewport-commit', {
        detail: { from: state.customFrom, to: state.customTo, source: 'gesture' },
    }));

    // When zooming or panning, fetch the optimal data resolution for the new view.
    // If we're already at max resolution (raw tier) and the buffer completely covers
    // the window, we can skip the network request.
    const fromDate = new Date(min);
    const toDate   = new Date(max);

    if (state.currentTier === 0 && !state.currentDownsampled && tryZoomFromBuffer(fromDate, toDate)) {
        releaseZoomPause();
        return;
    }

    state.loadingHistory = true;
    state.queueLiveDuringHistory = true;
    setHistoryStatus('loading');
    document.getElementById('loading-spinner')?.classList.remove('hidden');
    _zoomFetchTimer = setTimeout(() => {
        fetchZoomedHistory(fromDate, toDate);
    }, 150);
}

// Fetch higher-resolution data for a zoomed window and replace chart data,
// then re-apply the zoom so the viewport stays exactly where the user dragged.
export function fetchZoomedHistory(fromDate, toDate) {
    const points = historyPointBudget();
    return requestHistory(fromDate, toDate, points, (response, samples) => {
        const latest = replaceHistoryBuffer(samples, response?.resolution, response);
        updateLatestHistorySample(latest);

        // Re-apply the zoom viewport so the user stays in the same window.
        const minMs = fromDate.getTime();
        const maxMs = toDate.getTime();
        forEachRegisteredChart(chart => {
            if (!chart?.options?.scales?.x) return;
            chart.options.scales.x.min = minMs;
            chart.options.scales.x.max = maxMs;
        });
        queueAllChartUpdates();

        // Treat the zoomed window as a custom range so trimChartsToTimeRange
        // leaves the data alone, and live samples arriving after this point
        // won't clobber the viewport.
        state.timeRange = null;
        state.customFrom = fromDate;
        state.customTo = toDate;

        // The zoom is now "baked in" as an immutable exact range. Release the
        // transport pause; live samples continue updating status surfaces but
        // pushLiveSample keeps them out of historical datasets.
        releaseZoomPause();
    }, { label: 'Zoomed' }).finally(releaseZoomPause);
}

export function resetZoomAll() {
    forEachRegisteredChart(chart => {
        if (!chart?.options?.scales?.x) return;
        delete chart.options.scales.x.min;
        delete chart.options.scales.x.max;
        delete chart.options.scales.x.time.minUnit;
    });
    queueAllChartUpdates();

    // Resume from zoom-pause
    releaseZoomPause();
}

// ---- Gap Insertion ----
export function insertGapsInHistory(data, resolutionStr = '1s') {
    const items = data.map(normalizeHistoryItem).filter(Boolean);
    return insertHistoryGaps(items, resolutionStr);
}

export function addGapToCharts(marker, charts = null) {
    const start = new Date(marker?.gap_start ?? marker?.ts ?? marker).getTime();
    const end = new Date(marker?.gap_end).getTime();
    if (!charts && Number.isFinite(start) && Number.isFinite(end) && end > start) {
        const gaps = state.historyGaps || (state.historyGaps = []);
        const previous = gaps[gaps.length - 1];
        if (previous && start <= previous.end) {
            previous.end = Math.max(previous.end, end);
        } else {
            gaps.push({ start, end });
        }
    }

    const ts = Number.isFinite(start) ? start : new Date(marker).getTime();
    if (!Number.isFinite(ts)) return;
    const addGap = (chart) => {
        if (!chart?.data?.datasets) return;
        if (charts && !charts.has(chart)) return;
        chart.data.datasets.forEach(ds => {
            if (Array.isArray(ds.data)) appendEnvelopeGap(ds, ts);
        });
    };
    Object.values(state.charts).forEach(addGap);
    Object.values(state.splitCharts).forEach(typeCharts => {
        Object.values(typeCharts).forEach(addGap);
    });
    forEachAppChart(addGap);
}

// ---- Device Selectors ----
function updateDiskSelector(disks, type, selectionField, optionsField) {
    disks = [...disks].sort((a, b) => diskKey(a).localeCompare(diskKey(b)));
    const selected = migrateDiskSelection(state[selectionField], disks);
    const signature = JSON.stringify([selected, disks.map(d => [diskKey(d), d.name])]);
    state[selectionField] = selected;
    state[optionsField] = disks.map(diskKey);
    if (selected) localStorage.setItem(`kula_sel_${type}`, selected);
    const sel = document.getElementById(`${type}-selector`);
    if (!sel || state.diskSelectorSignatures[type] === signature) return;
    state.diskSelectorSignatures[type] = signature;
    sel.replaceChildren();
    for (const disk of disks) {
        const opt = document.createElement('option');
        opt.value = diskKey(disk);
        opt.textContent = diskLabel(disk);
        if (disk.id && disks.some(d => d !== disk && d.name === disk.name)) {
            opt.textContent += ` (${disk.id})`;
        }
        opt.title = diskTitle(disk);
        sel.appendChild(opt);
    }
    if (selected && !state[optionsField].includes(selected)) {
        const opt = document.createElement('option');
        opt.value = selected;
        opt.textContent = `${selected} (unavailable)`;
        sel.appendChild(opt);
    }
    sel.value = selected;
    const selectedDisk = diskMember(disks, selected);
    sel.title = selectedDisk ? diskTitle(selectedDisk) : selected;
    sel.classList.toggle('no-arrow', sel.options.length <= 1);
    sel.classList.remove('hidden');
    sel.onchange = e => {
        state[selectionField] = e.target.value;
        localStorage.setItem(`kula_sel_${type}`, state[selectionField]);
        const disk = diskMember(disks, state[selectionField]);
        sel.title = disk ? diskTitle(disk) : state[selectionField];
        redrawChartsFromBuffer([type]);
    };
}

export function updateSelectors(s) {
    const el = (id) => document.getElementById(id);

    if (s.net && s.net.ifaces) {
        const ifaces = s.net.ifaces.map(i => i.name).sort();
        if (ifaces.join(',') !== state.netOptions.join(',')) {
            state.netOptions = ifaces;
            const selNet = el('net-selector');
            const selPps = el('pps-selector');

            if (!state.selectedNet || !ifaces.includes(state.selectedNet)) {
                state.selectedNet = ifaces.find(i => i !== 'lo') || ifaces[0] || '';
                localStorage.setItem('kula_sel_net', state.selectedNet);
            }

            if (selNet) {
                selNet.innerHTML = '';
                ifaces.forEach(i => {
                    const opt = document.createElement('option');
                    opt.value = i;
                    opt.textContent = i;
                    selNet.appendChild(opt);
                });
                selNet.value = state.selectedNet;
                selNet.classList.toggle('no-arrow', ifaces.length <= 1);
                selNet.classList.remove('hidden');
                selNet.onchange = (e) => {
                    state.selectedNet = e.target.value;
                    if (selPps) selPps.value = state.selectedNet;
                    localStorage.setItem('kula_sel_net', state.selectedNet);
                    redrawChartsFromBuffer(['network', 'pps']);
                };
            }

            if (selPps) {
                selPps.innerHTML = '';
                ifaces.forEach(i => {
                    const opt = document.createElement('option');
                    opt.value = i;
                    opt.textContent = i;
                    selPps.appendChild(opt);
                });
                selPps.value = state.selectedNet;
                selPps.classList.toggle('no-arrow', ifaces.length <= 1);
                selPps.classList.remove('hidden');
                selPps.onchange = (e) => {
                    state.selectedNet = e.target.value;
                    if (selNet) selNet.value = state.selectedNet;
                    localStorage.setItem('kula_sel_net', state.selectedNet);
                    redrawChartsFromBuffer(['network', 'pps']);
                };
            }
        }
    }

    if (s.disk?.devices) {
        state.diskDevices = new Map(s.disk.devices.map(d => [diskKey(d), d]));
        updateDiskSelector(s.disk.devices, 'diskio', 'selectedDiskIo', 'diskIoOptions');
        updateDiskSelector(s.disk.devices.filter(d => d.temp > 0 || d.sensors?.length > 0),
            'disktemp', 'selectedDiskTemp', 'diskTempOptions');
    }

    if (s.disk && s.disk.filesystems) {
        const mounts = s.disk.filesystems.map(f => f.mount).sort();
        if (mounts.join(',') !== state.diskSpaceOptions.join(',')) {
            state.diskSpaceOptions = mounts;
            const sel = el('diskspace-selector');
            if (sel) {
                if (!state.selectedDiskSpace || !mounts.includes(state.selectedDiskSpace)) {
                    state.selectedDiskSpace = mounts.includes('/') ? '/' : (mounts[0] || '');
                    localStorage.setItem('kula_sel_diskspace', state.selectedDiskSpace);
                }
                sel.innerHTML = '';
                mounts.forEach(m => {
                    const opt = document.createElement('option');
                    opt.value = m;
                    opt.textContent = m;
                    sel.appendChild(opt);
                });
                sel.value = state.selectedDiskSpace;
                sel.classList.toggle('no-arrow', mounts.length <= 1);
                sel.classList.remove('hidden');
                sel.onchange = (e) => {
                    state.selectedDiskSpace = e.target.value;
                    localStorage.setItem('kula_sel_diskspace', state.selectedDiskSpace);
                    redrawChartsFromBuffer(['diskspace']);
                };
            }
        }
    }

    if (s.gpu && s.gpu.length > 0) {
        const gpus = s.gpu.map(g => g.name).sort();
        if (gpus.join(',') !== state.gpuLoadOptions.join(',')) {
            state.gpuLoadOptions = gpus;
            const selLoad = el('gpuload-selector');
            const selVram = el('vram-selector');
            const selTemp = el('gputemp-selector');

            if (!state.selectedGpuLoad || !gpus.includes(state.selectedGpuLoad)) {
                state.selectedGpuLoad = gpus[0];
                localStorage.setItem('kula_sel_gpuload', state.selectedGpuLoad);
            }

            [selLoad, selVram, selTemp].forEach(sel => {
                if (sel) {
                    sel.innerHTML = '';
                    gpus.forEach(g => {
                        const opt = document.createElement('option');
                        opt.value = g;
                        opt.textContent = g;
                        sel.appendChild(opt);
                    });
                    sel.value = state.selectedGpuLoad;
                    sel.classList.toggle('no-arrow', gpus.length <= 1);
                    sel.classList.remove('hidden');
                    sel.onchange = (e) => {
                        state.selectedGpuLoad = e.target.value;
                        if (selLoad) selLoad.value = state.selectedGpuLoad;
                        if (selVram) selVram.value = state.selectedGpuLoad;
                        if (selTemp) selTemp.value = state.selectedGpuLoad;
                        localStorage.setItem('kula_sel_gpuload', state.selectedGpuLoad);
                        redrawChartsFromBuffer(['gpuload', 'vram', 'gputemp']);
                    };
                }
            });
        }
    } else {
        const el = (id) => document.getElementById(id);
        ['gpuload-selector', 'vram-selector', 'gputemp-selector'].forEach(id => {
            el(id)?.classList.add('hidden');
        });
    }

    // Update split charts if device options changed
    updateSplitSelectors(s);
}

// ---- Live Sample Pipeline ----
// Push a single live sample — adds data + updates charts immediately
export function pushLiveSample(sample) {
    const item = annotateHistoryItems([sample], {
        tier: 0,
        resolution: 'live',
        complete: null,
        valid_aggregations: ['data'],
    })[0];
    const liveSample = historyItemSample(item);
    if (!liveSample) return;
    const ts = new Date(historyItemTimestamp(item));
    if (!Number.isFinite(ts.getTime())) return;

    // Learn cadence from live messages, including those already represented
    // by a history response. Historical timestamps must never become the
    // baseline for estimating the collection interval.
    if (state.lastLiveSampleTs === null || ts.getTime() > state.lastLiveSampleTs) {
        if (state.lastLiveSampleTs !== null) {
            const observed = updateLiveSampleInterval(
                state.liveSampleIntervals,
                ts.getTime() - state.lastLiveSampleTs,
            );
            state.liveSampleIntervals = observed.intervals;
            state.liveSampleIntervalMs = observed.estimate;
        }
        state.lastLiveSampleTs = ts.getTime();
    }

    // Prevent duplicate or out-of-order samples
    if (state.lastSample) {
        const lastTs = new Date(historyItemTimestamp(state.lastSample));
        if (ts.getTime() <= lastTs.getTime()) {
            return;
        }
    }

    state.lastSample = liveSample;
    updateGauges(liveSample);
    updateHeader(liveSample);

    // Exact custom/zoomed intervals are immutable evidence. Keep the live
    // status surfaces current, but do not append off-screen points to their
    // Chart.js datasets or canonical history buffer.
    if (state.timeRange === null && state.customFrom && state.customTo) {
        updateSubtitles(liveSample);
        evaluateAlerts(liveSample);
        return;
    }

    updateSelectors(liveSample);

    const refreshInterval = liveHistoryRefreshInterval(
        state.timeRange, state.historyPointLimit, state.liveSampleIntervalMs);
    if (refreshInterval > 0 || state.historyViewEnd !== null) {
        refreshRollingHistory(refreshInterval || 1000);
        updateSubtitles(liveSample);
        evaluateAlerts(liveSample);
        return;
    }

    trimChartsToTimeRange();
    if (state.dataBuffer.length >= state.historyPointLimit) {
        // The existing representation stays visible while a bounded full-range
        // response is fetched. Failed refreshes never delete its left edge.
        refreshRollingHistory(1000);
        updateSubtitles(liveSample);
        evaluateAlerts(liveSample);
        return;
    }
    state.dataBuffer.push(item);
    const context = historyItemContext(item);
    if (context) state.historyPointContexts.set(ts.getTime(), context);

    addSampleToCharts(item, ts);
    trimChartsToTimeRange();
    updateAllCharts();
    updateSubtitles(liveSample);
    evaluateAlerts(liveSample);
}

function applyAggregationValidity(validAggregations) {
    const resolved = resolveAggregation(state.currentAggregation, validAggregations);
    state.validAggregations = resolved.valid;

    if (resolved.selection !== state.currentAggregation) {
        state.currentAggregation = resolved.selection;

        // An old bookmark may still request agg=min|max. Once the server has
        // declared that operation invalid, remove the misleading URL state.
        const params = new URLSearchParams(window.location.search);
        if (params.has('agg')) {
            params.delete('agg');
            const query = params.toString();
            const url = window.location.pathname + (query ? `?${query}` : '') + window.location.hash;
            window.history.replaceState(null, '', url);
        }
    }

    document.querySelectorAll('#agg-presets-list .time-btn').forEach(button => {
        const field = aggregationField(button.dataset.agg);
        const valid = state.validAggregations.includes(field);
        button.classList.toggle('hidden', !valid);
        button.disabled = !valid;
        button.classList.toggle('active', button.dataset.agg === state.currentAggregation);
    });

    const hasChoice = state.validAggregations.some(field => field !== 'data');
    document.getElementById('agg-presets-list')?.classList.toggle('hidden', !hasChoice);
    document.getElementById('agg-divider')?.classList.toggle('hidden', !hasChoice);
    document.getElementById('btn-agg-menu')?.classList.toggle('hidden', !hasChoice);
    document.dispatchEvent(new Event('kula-history-metadata-changed'));
}

export function renderSamplingInfo(status = state.historyStatus) {
    const el = document.getElementById('sampling-info');
    if (!el) return;
    const tier = Number.isInteger(state.currentTier) && state.currentTier >= 0
        ? `${i18n.t('tier')} ${state.currentTier}` : '';
    const label = { failed: 'history_failed', partial: 'partial_range', empty: 'history_empty' }[status];
    el.textContent = [state.currentResolution, tier, label ? i18n.t(label) : ''].filter(Boolean).join(' · ');
}

export function updateSamplingInfo(
    tier,
    resolution,
    complete = true,
    validAggregations = ['data'],
    sourceResolution = resolution,
    downsampled = null,
) {
    state.currentResolution = resolution || '1s';
    state.currentSourceResolution = sourceResolution || state.currentResolution;
    state.currentDownsampled = typeof downsampled === 'boolean'
        ? downsampled
        : state.currentResolution !== state.currentSourceResolution;
    state.currentTier = tier;
    const minRange = minimumZoomSpan(state.currentSourceResolution, state.collectionIntervalMs);
    forEachRegisteredChart(chart => {
        if (chart.options?.plugins?.zoom?.limits?.x) {
            chart.options.plugins.zoom.limits.x.minRange = minRange;
        }
    });
    applyAggregationValidity(validAggregations);

    const el = document.getElementById('sampling-info');
    if (!el) return;
    const name = Number.isInteger(tier) && tier >= 0 ? `${i18n.t('tier')} ${tier}` : '';
    renderSamplingInfo(complete === false ? 'partial' : 'complete');
    const source = state.currentDownsampled
        ? `${i18n.t('source')}: ${state.currentSourceResolution}`
        : '';
    el.title = [name, source, state.timeZone === 'utc' ? 'UTC' : i18n.t('time_zone_local')]
        .filter(Boolean).join(' · ');
}

function refreshRollingHistory(interval) {
    const now = Date.now();
    if (state.historyViewEnd === null) {
        const end = Date.parse(state.historyCoverage?.requestedTo);
        state.historyViewEnd = Number.isFinite(end) ? end : now;
    }
    if (state.loadingHistory || now - Math.max(state.historyRefreshAttempt, state.historyViewEnd) < interval) return;
    void fetchHistory(state.timeRange, { background: true });
}

export function fetchHistory(rangeSeconds, { background = false } = {}) {
    const toDate = new Date();
    const fromDate = new Date(toDate.getTime() - rangeSeconds * 1000);
    const points = historyPointBudget();
    state.historyRefreshAttempt = toDate.getTime();
    return requestHistory(fromDate, toDate, points, (response, samples) => {
        const interval = liveHistoryRefreshInterval(rangeSeconds, points, state.liveSampleIntervalMs);
        state.historyViewEnd = interval > 0 ? toDate.getTime() : null;
        const latest = replaceHistoryBuffer(samples, response?.resolution, response);
        trimChartsToTimeRange();
        updateAllCharts();
        updateLatestHistorySample(latest);
        const info = document.getElementById('sampling-info');
        if (info && interval > 0) {
            info.title += ` · ${i18n.t('history_refresh_every')} ${Math.ceil(interval / 1000)}s` +
                ` · ${i18n.t('updated')}: ${formatRangeTimestamp(toDate, state.timeZone, i18n.currentLang)}`;
        }
    }, { showSpinner: !background, queueLive: !background });
}

export function fetchCustomHistory(fromDate, toDate) {
    const points = historyPointBudget();
    return requestHistory(fromDate, toDate, points, (response, samples) => {
        const latest = replaceHistoryBuffer(samples, response?.resolution, response);
        setChartTimeRange();
        updateAllCharts();
        updateLatestHistorySample(latest);
    }, { label: 'Custom' });
}


// Replay any samples that arrived while history was loading.
export function drainLiveQueue() {
    if (state.liveQueue.length === 0) return;
    const queue = state.liveQueue;
    state.liveQueue = [];
    queue.forEach(item => {
        // Skip samples whose timestamp was already covered by the history load
        if (state.lastHistoricalTs && new Date(historyItemTimestamp(item)) <= state.lastHistoricalTs) return;
        pushLiveSample(item);
    });
}
