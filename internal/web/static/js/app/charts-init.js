/* ============================================================
   charts-init.js — Chart.js instance creation and full
   dashboard chart initialization.
   ============================================================ */
'use strict';

// ---- Chart Initialization ----
function createTimeSeriesChart(canvasId, datasets, yConfig = {}, extraPlugins = {}) {
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
                    pan: { enabled: true, mode: 'x' },
                    zoom: {
                        drag: { enabled: true, backgroundColor: 'rgba(59,130,246,0.1)', borderColor: colors.blue, borderWidth: 1 },
                        mode: 'x',
                        onZoom: ({ chart }) => {
                            syncZoom(chart);
                            if (!state.pausedZoom) {
                                state.pausedZoom = true;
                                syncPauseState();
                            }
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
                    time: { tooltipFormat: 'HH:mm:ss', displayFormats: { second: 'HH:mm:ss', minute: 'HH:mm', hour: 'HH:mm' } },
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

function destroyAllCharts() {
    Object.keys(state.charts).forEach(key => {
        if (state.charts[key]) {
            state.charts[key].destroy();
            state.charts[key] = null;
        }
    });
}

function initCharts() {
    destroyAllCharts();

    // CPU
    state.charts.cpu = createTimeSeriesChart('chart-cpu', [
        { label: 'User', borderColor: colors.blue, backgroundColor: colors.blueAlpha, fill: true, data: [] },
        { label: 'System', borderColor: colors.red, backgroundColor: colors.redAlpha, fill: true, data: [] },
        { label: 'IOWait', borderColor: colors.yellow, backgroundColor: colors.yellowAlpha, fill: true, data: [] },
        { label: 'Steal', borderColor: colors.purple, backgroundColor: colors.purpleAlpha, fill: true, data: [] },
        { label: 'Total', borderColor: colors.cyan, data: [], fill: false, borderWidth: 2 },
    ], { max: 100, ticks: { callback: v => v + '%' } });

    state.cpuTempSensorNames = [];
    let cpuTempYConfig = { ticks: { callback: v => v.toFixed(1) + '°C' } };
    let cpuTempMax = getChartMaxBound('cpu_temp');
    if (cpuTempMax !== undefined) cpuTempYConfig.max = cpuTempMax;

    state.charts.cputemp = createTimeSeriesChart('chart-cpu-temp', [
        { label: 'Temperature', borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true, data: [] },
    ], cpuTempYConfig);

    // Load Average
    state.charts.loadavg = createTimeSeriesChart('chart-loadavg', [
        { label: '1 min', borderColor: colors.red, data: [], fill: false, borderWidth: 2 },
        { label: '5 min', borderColor: colors.yellow, data: [], fill: false },
        { label: '15 min', borderColor: colors.green, data: [], fill: false },
    ]);

    // Memory — with Free, Available, and Shmem datasets, max set dynamically
    state.charts.memory = createTimeSeriesChart('chart-memory', [
        { label: 'Used', borderColor: colors.blue, backgroundColor: colors.blueAlpha, fill: true, data: [] },
        { label: 'Buffers', borderColor: colors.cyan, backgroundColor: colors.cyanAlpha, fill: true, data: [] },
        { label: 'Cached', borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
        { label: 'Shmem', borderColor: colors.purple, backgroundColor: colors.purpleAlpha, fill: true, data: [] },
        { label: 'Free', borderColor: colors.teal, data: [], fill: false, borderDash: [4, 2] },
        { label: 'Available', borderColor: colors.lime, data: [], fill: false, borderDash: [4, 2] },
    ], { ticks: { callback: v => formatBytesShort(v) } }, {
        tooltip: { callbacks: { label: ctx => ctx.dataset.label + ': ' + formatBytesShort(ctx.parsed.y) } }
    });

    // Swap — with Free dataset, max set dynamically
    state.charts.swap = createTimeSeriesChart('chart-swap', [
        { label: 'Used', borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true, data: [] },
        { label: 'Free', borderColor: colors.teal, data: [], fill: false, borderDash: [4, 2] },
    ], { min: 0, ticks: { callback: v => formatBytesShort(v) } }, {
        tooltip: { callbacks: { label: ctx => ctx.dataset.label + ': ' + formatBytesShort(ctx.parsed.y) } }
    });

    let networkYConfig = { ticks: { callback: v => v.toFixed(1) + ' Mbps' } };
    let networkMax = getChartMaxBound('network');
    if (networkMax !== undefined) networkYConfig.max = networkMax;

    state.charts.network = createTimeSeriesChart('chart-network', [
        { label: '↓ RX', borderColor: colors.cyan, backgroundColor: colors.cyanAlpha, fill: true, data: [] },
        { label: '↑ TX', borderColor: colors.pink, backgroundColor: colors.pinkAlpha, fill: true, data: [] },
    ], networkYConfig);

    state.charts.pps = createTimeSeriesChart('chart-pps', [
        { label: '↓ RX pps', borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
        { label: '↑ TX pps', borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true, data: [] },
    ], { ticks: { callback: v => formatPPS(v) } }, {
        tooltip: { callbacks: { label: ctx => ctx.dataset.label + ': ' + formatPPS(Math.round(ctx.parsed.y)) } }
    });

    // Connections
    state.charts.connections = createTimeSeriesChart('chart-connections', [
        { label: 'TCP', borderColor: colors.blue, data: [], fill: false },
        { label: 'UDP', borderColor: colors.green, data: [], fill: false },
        { label: 'TIME_WAIT', borderColor: colors.yellow, data: [], fill: false },
        { label: 'Established', borderColor: colors.cyan, data: [], fill: false },
        { label: 'InErrs', borderColor: colors.red, data: [], fill: false, borderDash: [4, 2] },
        { label: 'OutRsts', borderColor: colors.orange, data: [], fill: false, borderDash: [4, 2] },
    ]);

    state.charts.diskio = createTimeSeriesChart('chart-disk-io', [
        { label: 'Read B/s', borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [], yAxisID: 'y' },
        { label: 'Write B/s', borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true, data: [], yAxisID: 'y' },
        { label: 'Reads/s', borderColor: colors.cyan, data: [], fill: false, borderDash: [4, 2], yAxisID: 'y1' },
        { label: 'Writes/s', borderColor: colors.pink, data: [], fill: false, borderDash: [4, 2], yAxisID: 'y1' },
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
        { label: 'Temperature', borderColor: colors.red, backgroundColor: colors.redAlpha, fill: true, data: [] },
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
        { label: 'Running', borderColor: colors.green, data: [], fill: false },
        { label: 'Sleeping', borderColor: colors.blue, data: [], fill: false },
        { label: 'Blocked', borderColor: colors.red, data: [], fill: false },
        { label: 'Zombie', borderColor: colors.yellow, data: [], fill: false },
        { label: 'Total', borderColor: colors.cyan, data: [], fill: false, borderDash: [4, 2] },
    ]);

    // Entropy
    state.charts.entropy = createTimeSeriesChart('chart-entropy', [
        { label: 'Entropy', borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
    ]);

    // GPU Load
    state.charts.gpuload = createTimeSeriesChart('chart-gpu-load', [
        { label: 'Load %', borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
        { label: 'Power W', borderColor: colors.orange, data: [], fill: false, yAxisID: 'y1' },
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
        { label: 'VRAM Used', borderColor: colors.purple, backgroundColor: colors.purpleAlpha, fill: true, data: [] },
    ], { ticks: { callback: v => formatBytesShort(v) } }, {
        tooltip: { callbacks: { label: ctx => ctx.dataset.label + ': ' + formatBytesShort(ctx.parsed.y) } }
    });

    // GPU Temperature
    let gpuTempMax = getChartMaxBound('gpu_temp');
    let gpuTempYConfig = { max: gpuTempMax, ticks: { callback: v => v.toFixed(1) + '°C' } };
    state.charts.gputemp = createTimeSeriesChart('chart-gpu-temp', [
        { label: 'Temperature', borderColor: colors.red, backgroundColor: colors.redAlpha, fill: true, data: [] },
    ], gpuTempYConfig);

    // Self monitoring
    state.charts.self = createTimeSeriesChart('chart-self', [
        { label: 'CPU %', borderColor: colors.cyan, data: [], fill: false, yAxisID: 'y' },
        { label: 'RSS', borderColor: colors.purple, data: [], fill: false, yAxisID: 'y1' },
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
function setChartTimeRange() {
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

    Object.values(state.charts).forEach(chart => {
        if (!chart?.options?.scales?.x || chart.config?.type === 'bar') return;
        chart.options.scales.x.min = xMin;
        chart.options.scales.x.max = xMax;
    });
}
