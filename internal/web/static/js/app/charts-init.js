/* ============================================================
   charts-init.js — Chart.js instance creation and full
   dashboard chart initialization.
   ============================================================ */
'use strict';
import { state, colors, getChartMaxBound } from './state.js';
import { formatBytesShort, formatPPS } from './utils.js';
import { i18n } from './i18n.js';

// ---- Chart Initialization ----
export function createTimeSeriesChart(canvasId, datasets, yConfig = {}, extraPlugins = {}) {
    const ctx = document.getElementById(canvasId);
    if (!ctx) return null;

    const chart = new Chart(ctx, {
        type: 'line',
        data: { datasets },
        options: {
            responsive: true,
            maintainAspectRatio: false,
            normalized: true,
            animation: false,
            interaction: { mode: 'index', intersect: false },
            spanGaps: state.joinMetrics,
            plugins: {
                legend: { position: 'top', align: 'end' },
                zoom: {
                    pan: {
                        enabled: true,
                        mode: 'x',
                        onPanStart: function({ chart }) { if (state.timeRange !== null) return false; },
                        onPan: function({ chart }) { document.dispatchEvent(new CustomEvent('kula-zoom-sync', { detail: chart })); },
                        onPanComplete: function({ chart }) {
                            document.dispatchEvent(new CustomEvent('kula-zoom-sync', { detail: chart }));
                            document.dispatchEvent(new Event('kula-sync-pause'));
                        },
                    },
                    zoom: {
                        drag: { enabled: true, backgroundColor: 'rgba(59,130,246,0.1)', borderColor: colors.blue, borderWidth: 1 },
                        mode: 'x',
                        onZoomStart: function({ chart }) { if (state.timeRange !== null) return false; },
                        onZoom: function({ chart }) { document.dispatchEvent(new CustomEvent('kula-zoom-sync', { detail: chart })); },
                        onZoomComplete: function({ chart }) {
                            document.dispatchEvent(new CustomEvent('kula-zoom-sync', { detail: chart }));
                            document.dispatchEvent(new Event('kula-sync-pause'));
                        },
                    },
                },
                tooltip: Object.assign(
                    { position: 'awayFromCursor' },
                    extraPlugins.tooltip || {}
                ),
            },
            scales: {
                x: {
                    type: 'time',
                    time: { 
                        tooltipFormat: 'MMM d, HH:mm:ss', 
                        displayFormats: { 
                            second: 'HH:mm:ss', 
                            minute: 'HH:mm', 
                            hour: 'HH:mm',
                            day: 'MMM d'
                        } 
                    },
                    grid: { display: false },
                    ticks: { maxTicksLimit: 8 },
                },
                y: {
                    beginAtZero: true,
                    ...yConfig,
                    grid: { color: 'rgba(55, 65, 81, 0.2)' },
                },
            },
            elements: {
                point: { radius: 0, hoverRadius: 3 },
                line: { tension: 0.3, borderWidth: 1.5 },
            },
        },
    });

    return chart;
}

export function destroyAllCharts() {
    Object.keys(state.charts).forEach(key => {
        if (state.charts[key]) {
            state.charts[key].destroy();
            state.charts[key] = null;
        }
    });
}

export function initCharts() {
    destroyAllCharts();

    // CPU
    state.charts.cpu = createTimeSeriesChart('chart-cpu', [
        { label: i18n.t('user'), borderColor: colors.blue, backgroundColor: colors.blueAlpha, fill: true, data: [] },
        { label: i18n.t('system'), borderColor: colors.red, backgroundColor: colors.redAlpha, fill: true, data: [] },
        { label: i18n.t('iowait'), borderColor: colors.yellow, backgroundColor: colors.yellowAlpha, fill: true, data: [] },
        { label: i18n.t('steal'), borderColor: colors.purple, backgroundColor: colors.purpleAlpha, fill: true, data: [] },
        { label: i18n.t('total'), borderColor: colors.cyan, data: [], fill: false, borderWidth: 2 },
    ], { max: 100, ticks: { callback: v => v + '%' } });

    state.cpuTempSensorNames = [];
    let cpuTempYConfig = { ticks: { callback: v => v.toFixed(1) + '°C' } };
    let cpuTempMax = getChartMaxBound('cpu_temp');
    if (cpuTempMax !== undefined) cpuTempYConfig.max = cpuTempMax;

    state.charts.cputemp = createTimeSeriesChart('chart-cpu-temp', [
        { label: i18n.t('temperature'), borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true, data: [] },
    ], cpuTempYConfig);

    // Load Average
    state.charts.loadavg = createTimeSeriesChart('chart-loadavg', [
        { label: i18n.t('1_min'), borderColor: colors.red, data: [], fill: false, borderWidth: 2 },
        { label: i18n.t('5_min'), borderColor: colors.yellow, data: [], fill: false },
        { label: i18n.t('15_min'), borderColor: colors.green, data: [], fill: false },
    ]);

    // Memory — with Free, Available, and Shmem datasets, max set dynamically
    state.charts.memory = createTimeSeriesChart('chart-memory', [
        { label: i18n.t('used'), borderColor: colors.blue, backgroundColor: colors.blueAlpha, fill: true, data: [] },
        { label: i18n.t('buffers'), borderColor: colors.cyan, backgroundColor: colors.cyanAlpha, fill: true, data: [] },
        { label: i18n.t('cached'), borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
        { label: i18n.t('shmem'), borderColor: colors.purple, backgroundColor: colors.purpleAlpha, fill: true, data: [] },
        { label: i18n.t('free'), borderColor: colors.teal, data: [], fill: false, borderDash: [4, 2] },
        { label: i18n.t('available'), borderColor: colors.lime, data: [], fill: false, borderDash: [4, 2] },
    ], { ticks: { callback: v => formatBytesShort(v) } }, {
        tooltip: { callbacks: { label: ctx => ctx.dataset.label + ': ' + formatBytesShort(ctx.parsed.y) } }
    });

    // Swap — with Free dataset, max set dynamically
    state.charts.swap = createTimeSeriesChart('chart-swap', [
        { label: i18n.t('used'), borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true, data: [] },
        { label: i18n.t('free'), borderColor: colors.teal, data: [], fill: false, borderDash: [4, 2] },
    ], { min: 0, ticks: { callback: v => formatBytesShort(v) } }, {
        tooltip: { callbacks: { label: ctx => ctx.dataset.label + ': ' + formatBytesShort(ctx.parsed.y) } }
    });

    let networkYConfig = { ticks: { callback: v => v.toFixed(1) + ' Mbps' } };
    let networkMax = getChartMaxBound('network');
    if (networkMax !== undefined) networkYConfig.max = networkMax;

    state.charts.network = createTimeSeriesChart('chart-network', [
        { label: i18n.t('rx'), borderColor: colors.cyan, backgroundColor: colors.cyanAlpha, fill: true, data: [] },
        { label: i18n.t('tx'), borderColor: colors.pink, backgroundColor: colors.pinkAlpha, fill: true, data: [] },
    ], networkYConfig);

    state.charts.pps = createTimeSeriesChart('chart-pps', [
        { label: i18n.t('rx_pps'), borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
        { label: i18n.t('tx_pps'), borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true, data: [] },
    ], { ticks: { callback: v => formatPPS(v) } }, {
        tooltip: { callbacks: { label: ctx => ctx.dataset.label + ': ' + formatPPS(Math.round(ctx.parsed.y)) } }
    });

    // Connections
    state.charts.connections = createTimeSeriesChart('chart-connections', [
        { label: i18n.t('tcp'), borderColor: colors.blue, data: [], fill: false },
        { label: i18n.t('udp'), borderColor: colors.green, data: [], fill: false },
        { label: i18n.t('time_wait'), borderColor: colors.yellow, data: [], fill: false },
        { label: i18n.t('established'), borderColor: colors.cyan, data: [], fill: false },
        { label: i18n.t('inerrs'), borderColor: colors.red, data: [], fill: false, borderDash: [4, 2] },
        { label: i18n.t('outrsts'), borderColor: colors.orange, data: [], fill: false, borderDash: [4, 2] },
    ]);

    state.charts.diskio = createTimeSeriesChart('chart-disk-io', [
        { label: i18n.t('read_bs'), borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [], yAxisID: 'y' },
        { label: i18n.t('write_bs'), borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true, data: [], yAxisID: 'y' },
        { label: i18n.t('reads_s'), borderColor: colors.cyan, data: [], fill: false, borderDash: [4, 2], yAxisID: 'y1' },
        { label: i18n.t('writes_s'), borderColor: colors.pink, data: [], fill: false, borderDash: [4, 2], yAxisID: 'y1' },
    ], { ticks: { callback: v => formatBytesShort(v) + '/s' } }, {
        tooltip: {
            callbacks: {
                label: ctx => ctx.dataset.yAxisID === 'y1'
                    ? ctx.dataset.label + ': ' + ctx.parsed.y.toFixed(0) + ' IOPS'
                    : ctx.dataset.label + ': ' + formatBytesShort(Math.round(ctx.parsed.y)) + '/s'
            }
        }
    });

    // Reconfigure disk IO chart for dual axes
    if (state.charts.diskio) {
        state.charts.diskio.options.scales.y1 = {
            position: 'right',
            beginAtZero: true,
            grid: { display: false },
            ticks: { callback: v => v.toFixed(0) + ' IO/s' },
        };
        state.charts.diskio.update('none');
    }

    state.diskTempSensorNames = [];
    let diskTempYConfig = { ticks: { callback: v => v.toFixed(1) + '°C' } };
    let diskTempMax = getChartMaxBound('disk_temp');
    if (diskTempMax !== undefined) diskTempYConfig.max = diskTempMax;

    state.charts.disktemp = createTimeSeriesChart('chart-disk-temp', [
        { label: i18n.t('temperature'), borderColor: colors.red, backgroundColor: colors.redAlpha, fill: true, data: [] },
    ], diskTempYConfig);

    // Disk Space — datasets are added dynamically per mount on first sample
    state.diskSpaceMountNames = [];
    state.charts.diskspace = createTimeSeriesChart('chart-disk-space', [],
        { max: 100, ticks: { callback: v => Math.round(v) + '%' } }, {
        tooltip: {
            callbacks: {
                label: ctx => {
                    const raw = ctx.raw;
                    if (raw && raw.used !== undefined && raw.total !== undefined) {
                        return `${ctx.dataset.label}: ${ctx.parsed.y.toFixed(1)}% (${formatBytesShort(raw.used)} / ${formatBytesShort(raw.total)})`;
                    }
                    return ctx.dataset.label + ': ' + ctx.parsed.y.toFixed(1) + '%';
                }
            }
        }
    });

    // Processes
    state.charts.processes = createTimeSeriesChart('chart-processes', [
        { label: i18n.t('running'), borderColor: colors.green, data: [], fill: false },
        { label: i18n.t('sleeping'), borderColor: colors.blue, data: [], fill: false },
        { label: i18n.t('blocked'), borderColor: colors.red, data: [], fill: false },
        { label: i18n.t('zombie'), borderColor: colors.yellow, data: [], fill: false },
        { label: i18n.t('total'), borderColor: colors.cyan, data: [], fill: false, borderDash: [4, 2] },
    ]);

    // Entropy
    state.charts.entropy = createTimeSeriesChart('chart-entropy', [
        { label: i18n.t('entropy'), borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
    ]);

    // GPU Load
    state.charts.gpuload = createTimeSeriesChart('chart-gpu-load', [
        { label: i18n.t('load_pct'), borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
        { label: i18n.t('power_w'), borderColor: colors.orange, data: [], fill: false, yAxisID: 'y1' },
    ], { max: 100, ticks: { callback: v => v + '%' } });
    if (state.charts.gpuload) {
        state.charts.gpuload.options.scales.y1 = {
            position: 'right',
            beginAtZero: true,
            grid: { display: false },
            ticks: { callback: v => v.toFixed(1) + ' W' },
        };
        state.charts.gpuload.update('none');
    }

    // VRAM
    state.charts.vram = createTimeSeriesChart('chart-vram', [
        { label: i18n.t('used'), borderColor: colors.purple, backgroundColor: colors.purpleAlpha, fill: true, data: [] },
    ], { ticks: { callback: v => formatBytesShort(v) } }, {
        tooltip: { callbacks: { label: ctx => ctx.dataset.label + ': ' + formatBytesShort(ctx.parsed.y) } }
    });

    // GPU Temperature
    let gpuTempMax = getChartMaxBound('gpu_temp');
    let gpuTempYConfig = { max: gpuTempMax, ticks: { callback: v => v.toFixed(1) + '°C' } };
    state.charts.gputemp = createTimeSeriesChart('chart-gpu-temp', [
        { label: i18n.t('temperature'), borderColor: colors.red, backgroundColor: colors.redAlpha, fill: true, data: [] },
    ], gpuTempYConfig);

    // Self monitoring
    state.charts.self = createTimeSeriesChart('chart-self', [
        { label: i18n.t('cpu_pct'), borderColor: colors.cyan, data: [], fill: false, yAxisID: 'y' },
        { label: i18n.t('rss'), borderColor: colors.purple, data: [], fill: false, yAxisID: 'y1' },
    ], {}, {
        tooltip: {
            callbacks: {
                label: ctx => ctx.dataset.yAxisID === 'y1'
                    ? ctx.dataset.label + ': ' + formatBytesShort(ctx.parsed.y)
                    : ctx.dataset.label + ': ' + ctx.parsed.y.toFixed(1)
            }
        }
    });
    // Reconfigure self chart for dual axes
    if (state.charts.self) {
        state.charts.self.options.scales.y1 = {
            position: 'right',
            beginAtZero: true,
            grid: { display: false },
            ticks: { callback: v => formatBytesShort(v) },
        };
        state.charts.self.update('none');
    }
}

// ---- Set x-axis bounds for full time window ----
export function setChartTimeRange() {
    const now = Date.now();
    let xMin, xMax;

    if (state.timeRange !== null) {
        xMin = now - state.timeRange * 1000;
        xMax = now;
    } else if (state.customFrom && state.customTo) {
        xMin = state.customFrom.getTime();
        xMax = state.customTo.getTime();
    } else {
        return;
    }

    const windowSec = (xMax - xMin) / 1000;
    const minUnit = windowSec >= 259200 ? 'day' : false;

    Object.values(state.charts).forEach(chart => {
        if (!chart?.options?.scales?.x || chart.config?.type === 'bar') return;
        chart.options.scales.x.min = xMin;
        chart.options.scales.x.max = xMax;
        if (minUnit) {
            chart.options.scales.x.time.minUnit = minUnit;
        } else {
            delete chart.options.scales.x.time.minUnit;
        }
    });
}

export function updateChartLabels() {
    if (!state.charts) return;

    if (state.charts.cpu) {
        state.charts.cpu.data.datasets[0].label = i18n.t('user');
        state.charts.cpu.data.datasets[1].label = i18n.t('system');
        state.charts.cpu.data.datasets[2].label = i18n.t('iowait');
        state.charts.cpu.data.datasets[3].label = i18n.t('steal');
        state.charts.cpu.data.datasets[4].label = i18n.t('total');
    }

    if (state.charts.cputemp && state.cpuTempSensorNames.length === 0) {
        state.charts.cputemp.data.datasets[0].label = i18n.t('temperature');
    }

    if (state.charts.loadavg) {
        state.charts.loadavg.data.datasets[0].label = i18n.t('1_min');
        state.charts.loadavg.data.datasets[1].label = i18n.t('5_min');
        state.charts.loadavg.data.datasets[2].label = i18n.t('15_min');
    }

    if (state.charts.memory) {
        state.charts.memory.data.datasets[0].label = i18n.t('used');
        state.charts.memory.data.datasets[1].label = i18n.t('buffers');
        state.charts.memory.data.datasets[2].label = i18n.t('cached');
        state.charts.memory.data.datasets[3].label = i18n.t('shmem');
        state.charts.memory.data.datasets[4].label = i18n.t('free');
        state.charts.memory.data.datasets[5].label = i18n.t('available');
    }

    if (state.charts.swap) {
        state.charts.swap.data.datasets[0].label = i18n.t('used');
        state.charts.swap.data.datasets[1].label = i18n.t('free');
    }

    if (state.charts.network) {
        state.charts.network.data.datasets[0].label = i18n.t('rx');
        state.charts.network.data.datasets[1].label = i18n.t('tx');
    }

    if (state.charts.pps) {
        state.charts.pps.data.datasets[0].label = i18n.t('rx_pps');
        state.charts.pps.data.datasets[1].label = i18n.t('tx_pps');
    }

    if (state.charts.connections) {
        state.charts.connections.data.datasets[0].label = i18n.t('tcp');
        state.charts.connections.data.datasets[1].label = i18n.t('udp');
        state.charts.connections.data.datasets[2].label = i18n.t('time_wait');
        state.charts.connections.data.datasets[3].label = i18n.t('established');
        state.charts.connections.data.datasets[4].label = i18n.t('inerrs');
        state.charts.connections.data.datasets[5].label = i18n.t('outrsts');
    }

    if (state.charts.diskio) {
        state.charts.diskio.data.datasets[0].label = i18n.t('read_bs');
        state.charts.diskio.data.datasets[1].label = i18n.t('write_bs');
        state.charts.diskio.data.datasets[2].label = i18n.t('reads_s');
        state.charts.diskio.data.datasets[3].label = i18n.t('writes_s');
    }

    if (state.charts.disktemp && state.diskTempSensorNames.length === 0) {
        state.charts.disktemp.data.datasets[0].label = i18n.t('temperature');
    }

    if (state.charts.processes) {
        state.charts.processes.data.datasets[0].label = i18n.t('running');
        state.charts.processes.data.datasets[1].label = i18n.t('sleeping');
        state.charts.processes.data.datasets[2].label = i18n.t('blocked');
        state.charts.processes.data.datasets[3].label = i18n.t('zombie');
        state.charts.processes.data.datasets[4].label = i18n.t('total');
    }

    if (state.charts.entropy) {
        state.charts.entropy.data.datasets[0].label = i18n.t('entropy');
    }

    if (state.charts.gpuload) {
        state.charts.gpuload.data.datasets[0].label = i18n.t('load_pct');
        state.charts.gpuload.data.datasets[1].label = i18n.t('power_w');
    }

    if (state.charts.vram) {
        state.charts.vram.data.datasets[0].label = i18n.t('used');
    }

    if (state.charts.gputemp) {
        state.charts.gputemp.data.datasets[0].label = i18n.t('temperature');
    }

    if (state.charts.self) {
        state.charts.self.data.datasets[0].label = i18n.t('cpu_pct');
        state.charts.self.data.datasets[1].label = i18n.t('rss');
    }
}
