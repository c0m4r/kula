/* ============================================================
   Kula-Szpiegula Dashboard Application
   WebSocket-driven live monitoring with Chart.js
   ============================================================ */

(function () {
    'use strict';

    // ---- State ----
    const state = {
        ws: null,
        paused: false,
        pausedManual: false,
        pausedHover: false,
        pausedZoom: false,
        connected: false,
        charts: {},
        timeRange: 300, // seconds, null when custom range
        customFrom: null,
        customTo: null,
        dataBuffer: [],
        maxBufferSize: 3600, // 1 hour of 1s data
        reconnectDelay: 1000,
        reconnectTimer: null,
        historyLoaded: false,
        loadingHistory: false,
        alerts: [],
        alertDropdownOpen: false,
        infoDropdownOpen: false,
        timeDropdownOpen: false,
        layoutMode: localStorage.getItem('kula_layout') || 'grid',
        lastSample: null,
        joinMetrics: false, // fetched from server config
        focusMode: false,
        focusSelecting: false,
        focusVisible: JSON.parse(localStorage.getItem('kula_focus_visible') || 'null'),
        currentResolution: '1s', // resolution of data currently loaded in charts
        liveQueue: [],        // samples buffered while history is loading
        diskDeviceNames: [],  // device names matching diskutil chart datasets
    };

    // ---- Color Palette ----
    const colors = {
        blue: '#3b82f6',
        cyan: '#06b6d4',
        green: '#10b981',
        yellow: '#f59e0b',
        orange: '#f97316',
        red: '#ef4444',
        purple: '#8b5cf6',
        pink: '#ec4899',
        teal: '#14b8a6',
        lime: '#84cc16',
        blueAlpha: 'rgba(59, 130, 246, 0.15)',
        cyanAlpha: 'rgba(6, 182, 212, 0.15)',
        greenAlpha: 'rgba(16, 185, 129, 0.15)',
        redAlpha: 'rgba(239, 68, 68, 0.15)',
        purpleAlpha: 'rgba(139, 92, 246, 0.15)',
        yellowAlpha: 'rgba(245, 158, 11, 0.15)',
        orangeAlpha: 'rgba(249, 115, 22, 0.15)',
        pinkAlpha: 'rgba(236, 72, 153, 0.15)',
        tealAlpha: 'rgba(20, 184, 166, 0.15)',
        limeAlpha: 'rgba(132, 204, 22, 0.15)',
    };

    // ---- Chart.js Global Config ----
    Chart.defaults.color = '#94a3b8';
    Chart.defaults.borderColor = 'rgba(55, 65, 81, 0.3)';
    Chart.defaults.font.family = "'Inter', sans-serif";
    Chart.defaults.font.size = 11;
    Chart.defaults.animation = false; // disable all animations for performance
    Chart.defaults.plugins.legend.labels.usePointStyle = true;
    Chart.defaults.plugins.legend.labels.pointStyleWidth = 8;
    Chart.defaults.plugins.legend.labels.boxHeight = 6;



    // ---- Bar Gauge Drawing (alternative layout) ----
    function drawBarGauge(containerId, value, max, color) {
        const container = document.getElementById(containerId);
        if (!container) return;
        const pct = Math.min((value / max) * 100, 100);
        let fill = container.querySelector('.bar-gauge-fill');
        if (!fill) {
            container.innerHTML = `<div class="bar-gauge-container"><div class="bar-gauge-track"><div class="bar-gauge-fill"></div></div></div>`;
            fill = container.querySelector('.bar-gauge-fill');
        }
        fill.style.width = pct + '%';
        // Set gradient
        if (Array.isArray(color)) {
            fill.style.background = `linear-gradient(90deg, ${color.join(', ')})`;
        } else {
            fill.style.background = color;
        }
    }

    function updateGauges(sample) {
        const cpuPct = sample.cpu?.total?.usage || 0;
        const ramPct = sample.mem?.used_pct || 0;
        const swapPct = sample.swap?.used_pct || 0;
        const lavg = sample.lavg?.load1 || 0;
        const numCores = (sample.cpu?.cores?.length || 1);

        // Sum network across non-lo interfaces
        let dlMbps = 0, ulMbps = 0;
        if (sample.net?.ifaces) {
            sample.net.ifaces.forEach(i => {
                if (i.name !== 'lo') { dlMbps += i.rx_mbps || 0; ulMbps += i.tx_mbps || 0; }
            });
        }

        drawBarGauge('gauge-cpu-canvas', cpuPct, 100, [colors.green, colors.yellow, colors.red]);
        document.getElementById('gauge-cpu-value').textContent = cpuPct.toFixed(1) + '%';

        drawBarGauge('gauge-ram-canvas', ramPct, 100, [colors.cyan, colors.blue, colors.purple]);
        document.getElementById('gauge-ram-value').textContent = ramPct.toFixed(1) + '%';

        drawBarGauge('gauge-swap-canvas', swapPct, 100, [colors.teal, colors.orange, colors.red]);
        document.getElementById('gauge-swap-value').textContent = swapPct.toFixed(1) + '%';

        drawBarGauge('gauge-lavg-canvas', lavg, numCores * 2, [colors.green, colors.yellow, colors.red]);
        document.getElementById('gauge-lavg-value').textContent = lavg.toFixed(2);

        const maxNet = Math.max(dlMbps, ulMbps, 1);
        drawBarGauge('gauge-dl-canvas', dlMbps, Math.max(maxNet * 1.5, 10), [colors.cyan, colors.blue]);
        document.getElementById('gauge-dl-value').textContent = formatMbps(dlMbps);

        drawBarGauge('gauge-ul-canvas', ulMbps, Math.max(maxNet * 1.5, 10), [colors.pink, colors.purple]);
        document.getElementById('gauge-ul-value').textContent = formatMbps(ulMbps);
    }

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
                    tooltip: extraPlugins.tooltip || {},
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

        // Load Average
        state.charts.loadavg = createTimeSeriesChart('chart-loadavg', [
            { label: '1 min', borderColor: colors.red, data: [], fill: false, borderWidth: 2 },
            { label: '5 min', borderColor: colors.yellow, data: [], fill: false },
            { label: '15 min', borderColor: colors.green, data: [], fill: false },
        ]);

        // Memory — with Free and Available datasets, max set dynamically
        state.charts.memory = createTimeSeriesChart('chart-memory', [
            { label: 'Used', borderColor: colors.blue, backgroundColor: colors.blueAlpha, fill: true, data: [] },
            { label: 'Buffers', borderColor: colors.cyan, backgroundColor: colors.cyanAlpha, fill: true, data: [] },
            { label: 'Cached', borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
            { label: 'Free', borderColor: colors.teal, data: [], fill: false, borderDash: [4, 2] },
            { label: 'Available', borderColor: colors.lime, data: [], fill: false, borderDash: [4, 2] },
        ], { ticks: { callback: v => formatBytesShort(v) } });

        // Swap — with Free dataset, max set dynamically
        state.charts.swap = createTimeSeriesChart('chart-swap', [
            { label: 'Used', borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true, data: [] },
            { label: 'Free', borderColor: colors.teal, data: [], fill: false, borderDash: [4, 2] },
        ], { min: 0, ticks: { callback: v => formatBytesShort(v) } });

        // Network
        state.charts.network = createTimeSeriesChart('chart-network', [
            { label: '↓ RX', borderColor: colors.cyan, backgroundColor: colors.cyanAlpha, fill: true, data: [] },
            { label: '↑ TX', borderColor: colors.pink, backgroundColor: colors.pinkAlpha, fill: true, data: [] },
        ], { ticks: { callback: v => v.toFixed(1) + ' Mbps' } });

        // Packets per second
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
        ]);

        // Disk I/O
        state.charts.diskio = createTimeSeriesChart('chart-disk-io', [
            { label: 'Read', borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
            { label: 'Write', borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true, data: [] },
        ], { ticks: { callback: v => formatBytesShort(v) + '/s' } }, {
            tooltip: { callbacks: { label: ctx => ctx.dataset.label + ': ' + formatBytesShort(Math.round(ctx.parsed.y)) + '/s' } }
        });

        // Disk Utilization
        // Disk Utilization — datasets are added dynamically per device on first sample
        state.diskDeviceNames = [];
        state.charts.diskutil = createTimeSeriesChart('chart-disk-util', [],
            { max: 100, ticks: { callback: v => v + '%' } });

        // Disk Space (bar chart)
        state.charts.diskspace = new Chart(document.getElementById('chart-disk-space'), {
            type: 'bar',
            data: {
                labels: [], datasets: [
                    { label: 'Used', backgroundColor: colors.blue, data: [] },
                    { label: 'Available', backgroundColor: 'rgba(55, 65, 81, 0.4)', data: [] },
                ]
            },
            options: {
                responsive: true,
                maintainAspectRatio: false,
                indexAxis: 'y',
                plugins: { legend: { display: false } },
                scales: {
                    x: { stacked: true, ticks: { callback: v => formatBytesShort(v) } },
                    y: { stacked: true, grid: { display: false } },
                },
            },
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

        // Self monitoring
        state.charts.self = createTimeSeriesChart('chart-self', [
            { label: 'CPU %', borderColor: colors.cyan, data: [], fill: false, yAxisID: 'y' },
            { label: 'RSS MB', borderColor: colors.purple, data: [], fill: false, yAxisID: 'y1' },
        ], {});

        // Reconfigure self chart for dual axes
        if (state.charts.self) {
            state.charts.self.options.scales.y1 = {
                position: 'right',
                beginAtZero: true,
                grid: { display: false },
                ticks: { callback: v => v.toFixed(0) + ' MB' },
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

    // ---- Data Update ----
    function addSampleToCharts(item, ts) {
        const s = item.data || item;
        const point = (v) => ({ x: ts, y: v });

        // CPU
        if (state.charts.cpu && s.cpu?.total) {
            state.charts.cpu.data.datasets[0].data.push(point(s.cpu.total.user));
            state.charts.cpu.data.datasets[1].data.push(point(s.cpu.total.system));
            state.charts.cpu.data.datasets[2].data.push(point(s.cpu.total.iowait));
            state.charts.cpu.data.datasets[3].data.push(point(s.cpu.total.steal));

            // Show peak CPU for total usage to preserve spikes
            const usageVal = item.peak_cpu !== undefined ? item.peak_cpu : s.cpu.total.usage;
            state.charts.cpu.data.datasets[4].data.push(point(usageVal));
        }

        // Load Average
        if (state.charts.loadavg && s.lavg) {
            state.charts.loadavg.data.datasets[0].data.push(point(s.lavg.load1));
            state.charts.loadavg.data.datasets[1].data.push(point(s.lavg.load5));
            state.charts.loadavg.data.datasets[2].data.push(point(s.lavg.load15));
        }

        // Memory — with Free and Available
        if (state.charts.memory && s.mem) {
            state.charts.memory.data.datasets[0].data.push(point(s.mem.used));
            state.charts.memory.data.datasets[1].data.push(point(s.mem.buffers));
            state.charts.memory.data.datasets[2].data.push(point(s.mem.cached));
            state.charts.memory.data.datasets[3].data.push(point(s.mem.free));
            state.charts.memory.data.datasets[4].data.push(point(s.mem.available));
            // Set max to total RAM
            if (s.mem.total > 0) {
                state.charts.memory.options.scales.y.max = s.mem.total;
            }
        }

        // Swap — with Free
        if (state.charts.swap && s.swap) {
            state.charts.swap.data.datasets[0].data.push(point(s.swap.used || 0));
            state.charts.swap.data.datasets[1].data.push(point(s.swap.free || 0));
            // Set max to total swap
            if (s.swap.total > 0) {
                state.charts.swap.options.scales.y.max = s.swap.total;
            }
        }

        // Network (sum non-lo)
        if (state.charts.network && s.net?.ifaces) {
            let rx = 0, tx = 0;
            s.net.ifaces.forEach(i => { if (i.name !== 'lo') { rx += i.rx_mbps || 0; tx += i.tx_mbps || 0; } });

            const finalRx = item.peak_rx_mbps !== undefined ? item.peak_rx_mbps : rx;
            const finalTx = item.peak_tx_mbps !== undefined ? item.peak_tx_mbps : tx;

            state.charts.network.data.datasets[0].data.push(point(finalRx));
            state.charts.network.data.datasets[1].data.push(point(finalTx));
        }

        // Packets per second (sum non-lo)
        if (state.charts.pps && s.net?.ifaces) {
            let rxPps = 0, txPps = 0;
            s.net.ifaces.forEach(i => { if (i.name !== 'lo') { rxPps += i.rx_pps || 0; txPps += i.tx_pps || 0; } });
            state.charts.pps.data.datasets[0].data.push(point(rxPps));
            state.charts.pps.data.datasets[1].data.push(point(txPps));
        }

        // Connections
        if (state.charts.connections && s.net?.sockets) {
            state.charts.connections.data.datasets[0].data.push(point(s.net.sockets.tcp_inuse));
            state.charts.connections.data.datasets[1].data.push(point(s.net.sockets.udp_inuse));
            state.charts.connections.data.datasets[2].data.push(point(s.net.sockets.tcp_tw));
            state.charts.connections.data.datasets[3].data.push(point(s.net?.tcp?.curr_estab || 0));
        }

        // Disk I/O (sum all devices)
        if (state.charts.diskio && s.disk?.devices) {
            let rBps = 0, wBps = 0;
            s.disk.devices.forEach(d => { rBps += d.read_bps || 0; wBps += d.write_bps || 0; });
            state.charts.diskio.data.datasets[0].data.push(point(rBps));
            state.charts.diskio.data.datasets[1].data.push(point(wBps));
        }

        // Disk Utilization — one dataset per device, created dynamically
        if (state.charts.diskutil && s.disk?.devices && s.disk.devices.length > 0) {
            const diskColorPairs = [
                [colors.purple, colors.purpleAlpha],
                [colors.cyan, colors.cyanAlpha],
                [colors.orange, colors.orangeAlpha],
                [colors.green, colors.greenAlpha],
                [colors.pink, colors.pinkAlpha],
                [colors.yellow, colors.yellowAlpha],
                [colors.blue, colors.blueAlpha],
                [colors.teal, colors.tealAlpha],
            ];
            const incomingNames = s.disk.devices.map(d => d.name);
            if (incomingNames.join(',') !== state.diskDeviceNames.join(',')) {
                // Device set changed — rebuild datasets, preserving nothing
                state.diskDeviceNames = incomingNames;
                state.charts.diskutil.data.datasets = incomingNames.map((name, i) => ({
                    label: name,
                    borderColor: diskColorPairs[i % diskColorPairs.length][0],
                    backgroundColor: diskColorPairs[i % diskColorPairs.length][1],
                    fill: false,
                    data: [],
                }));
            }
            s.disk.devices.forEach((d, i) => {
                if (i < state.charts.diskutil.data.datasets.length) {
                    // For disk util we only have overall peak disk util across all devices.
                    // But maybe d.util_pct is fine, wait, item.peak_disk_util is overall.
                    // We can just use item.peak_disk_util for the highest utilized disk, 
                    // or just use d.util_pct since we don't have per-disk peaks.
                    state.charts.diskutil.data.datasets[i].data.push(point(d.util_pct));
                }
            });
        }

        // Disk Space (update bar chart — overwrite, not append)
        if (state.charts.diskspace && s.disk?.filesystems) {
            const labels = [];
            const used = [];
            const avail = [];
            s.disk.filesystems.forEach(f => {
                labels.push(f.mount);
                used.push(f.used);
                avail.push(f.available);
            });
            state.charts.diskspace.data.labels = labels;
            state.charts.diskspace.data.datasets[0].data = used;
            state.charts.diskspace.data.datasets[1].data = avail;
        }

        // Processes
        if (state.charts.processes && s.proc) {
            state.charts.processes.data.datasets[0].data.push(point(s.proc.running));
            state.charts.processes.data.datasets[1].data.push(point(s.proc.sleeping));
            state.charts.processes.data.datasets[2].data.push(point(s.proc.blocked));
            state.charts.processes.data.datasets[3].data.push(point(s.proc.zombie));
            state.charts.processes.data.datasets[4].data.push(point(s.proc.total));
        }

        // Entropy
        if (state.charts.entropy && s.sys) {
            state.charts.entropy.data.datasets[0].data.push(point(s.sys.entropy));
        }

        // Self
        if (state.charts.self && s.self) {
            state.charts.self.data.datasets[0].data.push(point(s.self.cpu_pct));
            state.charts.self.data.datasets[1].data.push(point((s.self.mem_rss || 0) / 1048576));
        }
    }

    // Batch-update all charts at once
    function updateAllCharts() {
        setChartTimeRange();
        Object.values(state.charts).forEach(chart => {
            if (chart) chart.update('none');
        });
    }

    // Push a single live sample — adds data + updates charts immediately
    function pushLiveSample(sample) {
        const ts = new Date(sample.ts);
        state.dataBuffer.push(sample);
        state.lastSample = sample;
        if (state.dataBuffer.length > state.maxBufferSize) {
            state.dataBuffer.shift();
        }

        updateGauges(sample);
        updateHeader(sample);
        addSampleToCharts(sample, ts); // For live sample, item is the sample itself
        trimChartsToTimeRange();
        updateAllCharts();
        updateSubtitles(sample);
        evaluateAlerts(sample);
    }

    function trimChartsToTimeRange() {
        if (state.timeRange === null) return; // custom range — don't trim
        const cutoffMs = Date.now() - state.timeRange * 1000;

        Object.values(state.charts).forEach(chart => {
            if (!chart || !chart.data?.datasets) return;
            chart.data.datasets.forEach(ds => {
                if (!Array.isArray(ds.data) || ds.data.length === 0) return;
                // Find the first index still within range, then splice once
                let i = 0;
                while (i < ds.data.length && ds.data[i].x && ds.data[i].x < cutoffMs) i++;
                if (i > 0) ds.data.splice(0, i);
            });
        });

        // Keep dataBuffer in sync with the displayed time window
        const cutoffDate = new Date(cutoffMs);
        let bi = 0;
        while (bi < state.dataBuffer.length && new Date(state.dataBuffer[bi].ts) < cutoffDate) bi++;
        if (bi > 0) state.dataBuffer.splice(0, bi);
    }

    function clearAllChartData() {
        Object.values(state.charts).forEach(chart => {
            if (!chart?.data?.datasets) return;
            chart.data.datasets.forEach(ds => {
                if (Array.isArray(ds.data)) ds.data = [];
            });
        });
    }

    function syncZoom(sourceChart) {
        const { min, max } = sourceChart.scales.x;

        // Update the display to show the zoomed timeframe explicitly
        if (min && max) {
            const fmt = d => d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' });
            document.getElementById('time-range-display').textContent = `${fmt(new Date(min))} → ${fmt(new Date(max))} (Zoomed)`;
        }

        Object.values(state.charts).forEach(chart => {
            if (!chart || chart === sourceChart || !chart.options?.scales?.x) return;
            chart.options.scales.x.min = min;
            chart.options.scales.x.max = max;
            chart.update('none');
        });

        // If current data is coarser than 1s and the zoomed window fits within
        // 1-hour of 1s samples, re-fetch at higher resolution for this window.
        if (min && max && state.currentResolution !== '1s') {
            const windowSec = (max - min) / 1000;
            if (windowSec <= 3600) {
                fetchZoomedHistory(new Date(min), new Date(max));
            }
        }
    }

    // Fetch higher-resolution data for a zoomed window and replace chart data,
    // then re-apply the zoom so the viewport stays exactly where the user dragged.
    function fetchZoomedHistory(fromDate, toDate) {
        if (state.loadingHistory) return;
        state.loadingHistory = true;
        document.getElementById('loading-spinner')?.classList.remove('hidden');

        const from = fromDate.toISOString();
        const to = toDate.toISOString();
        fetch(`/api/history?from=${from}&to=${to}`)
            .then(r => r.json())
            .then(response => {
                const data = response.samples || response;
                const isEnvelope = response.samples !== undefined;

                if (isEnvelope) {
                    updateSamplingInfo(response.tier, response.resolution);
                }

                clearAllChartData();
                state.dataBuffer = [];

                if (Array.isArray(data) && data.length > 0) {
                    const processed = insertGapsInHistory(data, response?.resolution);
                    processed.forEach(item => {
                        if (item._gap) {
                            addGapToCharts(new Date(item.ts));
                            return;
                        }
                        const sample = item.data || item;
                        const ts = new Date(sample.ts);
                        state.dataBuffer.push(sample);
                        addSampleToCharts(item, ts); // pass raw item to preserve peaks
                    });

                    if (state.dataBuffer.length > state.maxBufferSize) {
                        state.dataBuffer = state.dataBuffer.slice(-state.maxBufferSize);
                    }

                    const lastSample = data[data.length - 1];
                    const s = lastSample.data || lastSample;
                    state.lastSample = s;
                    updateGauges(s);
                    updateHeader(s);
                    updateSubtitles(s);
                    evaluateAlerts(s);
                }

                // Re-apply the zoom viewport so the user stays in the same window
                const minMs = fromDate.getTime();
                const maxMs = toDate.getTime();
                Object.values(state.charts).forEach(chart => {
                    if (!chart?.options?.scales?.x) return;
                    chart.options.scales.x.min = minMs;
                    chart.options.scales.x.max = maxMs;
                    chart.update('none');
                });

                // Treat the zoomed window as a custom range so trimChartsToTimeRange
                // leaves the data alone, and live samples arriving after this point
                // won't clobber the viewport.
                state.timeRange = null;
                state.customFrom = fromDate;
                state.customTo = toDate;

                // The zoom is now "baked in" as a custom range — release zoom-pause
                // so the WS stream resumes (new samples land outside the viewport
                // and are ignored visually until the user resets zoom).
                if (state.pausedZoom) {
                    state.pausedZoom = false;
                    syncPauseState();
                }

                state.loadingHistory = false;
                document.getElementById('loading-spinner')?.classList.add('hidden');
                drainLiveQueue();
            })
            .catch(e => {
                console.error('Zoomed history fetch error:', e);
                state.loadingHistory = false;
                document.getElementById('loading-spinner')?.classList.add('hidden');
                drainLiveQueue();
            });
    }

    function resetZoomAll() {
        Object.values(state.charts).forEach(chart => {
            if (!chart?.options?.scales?.x) return;
            delete chart.options.scales.x.min;
            delete chart.options.scales.x.max;
            chart.update('none');
        });

        // Restore time range display text
        if (state.timeRange !== null) {
            const labels = {
                60: 'Last 1 minute', 300: 'Last 5 minutes', 900: 'Last 15 minutes', 1800: 'Last 30 minutes',
                3600: 'Last 1 hour', 10800: 'Last 3 hours', 21600: 'Last 6 hours', 43200: 'Last 12 hours',
                86400: 'Last 24 hours', 259200: 'Last 3 days', 604800: 'Last 7 days', 2592000: 'Last 30 days'
            };
            document.getElementById('time-range-display').textContent = labels[state.timeRange] || `Last ${state.timeRange}s`;
        } else if (state.customFrom && state.customTo) {
            const fmt = d => d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
            document.getElementById('time-range-display').textContent = `${fmt(state.customFrom)} → ${fmt(state.customTo)}`;
        }

        // Resume from zoom-pause
        if (state.pausedZoom) {
            state.pausedZoom = false;
            syncPauseState();
        }
    }

    // ---- Alert System ----
    function evaluateAlerts(sample) {
        const alerts = [];
        const numCores = sample.cpu?.cores?.length || 1;

        // Clock not synced
        if (sample.sys?.clock_synced === false) {
            alerts.push({
                icon: '⏱',
                title: 'Clock not synchronized',
                detail: 'Source: ' + (sample.sys.clock_source || 'unknown'),
            });
        }

        // Low entropy
        if (sample.sys?.entropy !== undefined && sample.sys.entropy < 256) {
            alerts.push({
                icon: '🎲',
                title: 'Low entropy',
                detail: `Current: ${sample.sys.entropy} (min recommended: 256)`,
            });
        }

        // Load average exceeds core count
        if (sample.lavg?.load1 > numCores) {
            alerts.push({
                icon: '🔥',
                title: 'Load exceeds core count',
                detail: `Load1: ${sample.lavg.load1.toFixed(2)}, Cores: ${numCores}`,
            });
        }

        // CPU usage > 95%
        if (sample.cpu?.total?.usage > 95) {
            alerts.push({
                icon: '🔥',
                title: 'High CPU usage',
                detail: `CPU: ${sample.cpu.total.usage.toFixed(1)}%`,
            });
        }

        // RAM usage > 95%
        if (sample.mem?.used_pct > 95) {
            alerts.push({
                icon: '💾',
                title: 'High memory usage',
                detail: `RAM: ${sample.mem.used_pct.toFixed(1)}%`,
            });
        }

        // SWAP usage > 95%
        if (sample.swap?.used_pct > 95) {
            alerts.push({
                icon: '💾',
                title: 'High swap usage',
                detail: `Swap: ${sample.swap.used_pct.toFixed(1)}%`,
            });
        }

        state.alerts = alerts;
        updateAlertUI();
    }

    function updateAlertUI() {
        const badge = document.getElementById('alert-badge');
        const btn = document.getElementById('btn-alerts');
        const list = document.getElementById('alert-list');

        if (state.alerts.length > 0) {
            badge.textContent = state.alerts.length;
            badge.classList.remove('hidden');
            btn.classList.add('has-alerts');
            btn.classList.remove('no-alerts');
        } else {
            badge.classList.add('hidden');
            btn.classList.remove('has-alerts');
            btn.classList.add('no-alerts');
        }

        // Render alert items
        if (state.alerts.length === 0) {
            list.innerHTML = '<div class="alert-empty">No active alerts</div>';
        } else {
            list.innerHTML = state.alerts.map(a => `
                <div class="alert-item">
                    <span class="alert-icon">${a.icon}</span>
                    <div class="alert-item-body">
                        <div class="alert-item-title">${a.title}</div>
                        <div class="alert-item-detail">${a.detail}</div>
                    </div>
                </div>
            `).join('');
        }
    }

    function toggleAlertDropdown() {
        state.alertDropdownOpen = !state.alertDropdownOpen;
        const dropdown = document.getElementById('alert-dropdown');
        if (state.alertDropdownOpen) {
            dropdown.classList.remove('hidden');
        } else {
            dropdown.classList.add('hidden');
        }
    }

    function toggleInfoDropdown() {
        state.infoDropdownOpen = !state.infoDropdownOpen;
        const dropdown = document.getElementById('info-dropdown');
        if (state.infoDropdownOpen) {
            dropdown.classList.remove('hidden');
        } else {
            dropdown.classList.add('hidden');
        }
    }

    // ---- Header / Subtitles ----
    function updateHeader(s) {
        const el = (id) => document.getElementById(id);
        if (s.sys?.uptime_human) el('uptime').textContent = '⏱ ' + s.sys.uptime_human;
        el('clock').textContent = new Date(s.ts).toLocaleTimeString();

        // Helper to prevent XSS in innerHTML
        const escapeHTML = (str) => String(str).replace(/[&<>"']/g, m => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[m]));

        // System info footer — with colored clock sync
        const sysInfo = [];
        if (s.sys?.clock_synced !== undefined) {
            const synced = s.sys.clock_synced;
            const cls = synced ? 'clock-synced' : 'clock-not-synced';
            const label = synced ? '✓ synced' : '✗ not synced';
            sysInfo.push(`clock: <span class="${cls}">${label}</span>`);
        }
        if (s.sys?.clock_source) sysInfo.push('source: ' + escapeHTML(s.sys.clock_source));
        if (s.sys?.entropy) sysInfo.push('entropy: ' + escapeHTML(s.sys.entropy));
        if (s.sys?.user_count !== undefined) sysInfo.push('users: ' + escapeHTML(s.sys.user_count));
        if (s.self) sysInfo.push('self: ' + s.self.cpu_pct.toFixed(1) + '% cpu, ' + formatBytesShort(s.self.mem_rss) + ' rss');
        el('sys-info').innerHTML = sysInfo.map(text => `<span class="sys-info-item">${text}</span>`).join('<span class="sys-sep mobile-hidden">│</span>');
    }

    function updateSubtitles(s) {
        const el = (id, text) => { const e = document.getElementById(id); if (e) e.textContent = text; };

        if (s.cpu?.total) {
            el('cpu-subtitle', `usr:${s.cpu.total.user.toFixed(1)}% sys:${s.cpu.total.system.toFixed(1)}% io:${s.cpu.total.iowait.toFixed(1)}% ${s.cpu.cores?.length || 0} cores`);
        }
        if (s.lavg) el('lavg-subtitle', `${s.lavg.load1.toFixed(2)} / ${s.lavg.load5.toFixed(2)} / ${s.lavg.load15.toFixed(2)}`);
        // Memory — with % appended
        if (s.mem) {
            const memPct = s.mem.used_pct !== undefined ? s.mem.used_pct.toFixed(1) : '0.0';
            el('mem-subtitle', `${formatBytesShort(s.mem.used)} / ${formatBytesShort(s.mem.total)} (${memPct}%)`);
        }
        // Swap — with % appended
        if (s.swap) {
            const swapPct = s.swap.used_pct !== undefined ? s.swap.used_pct.toFixed(1) : '0.0';
            el('swap-subtitle', `${formatBytesShort(s.swap.used || 0)} / ${formatBytesShort(s.swap.total || 0)} (${swapPct}%)`);
        }
        if (s.net?.ifaces) {
            let rx = 0, tx = 0, rxPps = 0, txPps = 0;
            s.net.ifaces.forEach(i => {
                if (i.name !== 'lo') {
                    rx += i.rx_mbps || 0; tx += i.tx_mbps || 0;
                    rxPps += i.rx_pps || 0; txPps += i.tx_pps || 0;
                }
            });
            el('net-subtitle', `↓${formatMbps(rx)} ↑${formatMbps(tx)}`);
            el('pps-subtitle', `↓${formatPPS(rxPps)} ↑${formatPPS(txPps)}`);
        }
        if (s.disk?.devices) {
            let r = 0, w = 0;
            s.disk.devices.forEach(d => { r += d.read_bps || 0; w += d.write_bps || 0; });
            el('diskio-subtitle', `R:${formatBytesShort(r)}/s W:${formatBytesShort(w)}/s`);
        }
        if (s.proc) el('proc-subtitle', `${s.proc.total} total, ${s.proc.running} running`);
        if (s.self) el('self-subtitle', `${s.self.cpu_pct.toFixed(1)}% cpu, ${formatBytesShort(s.self.mem_rss)} rss`);
    }

    // ---- WebSocket ----
    function connectWS() {
        const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
        const wsUrl = `${proto}//${location.host}/ws`;

        try {
            state.ws = new WebSocket(wsUrl);
        } catch (e) {
            scheduleReconnect();
            return;
        }

        state.ws.onopen = () => {
            state.connected = true;
            state.reconnectDelay = 1000;
            updateConnectionStatus(true);
            // Load history for the current time window on first connect
            if (!state.historyLoaded) {
                state.historyLoaded = true;
                fetchHistory(state.timeRange);
            }
        };

        state.ws.onmessage = (evt) => {
            if (state.loadingHistory) {
                // Buffer samples that arrive while history is loading so there
                // is no gap when live streaming resumes after the fetch.
                try {
                    const sample = JSON.parse(evt.data);
                    state.liveQueue.push(sample);
                    if (state.liveQueue.length > 120) state.liveQueue.shift(); // cap at 2 min
                } catch (e) { /* ignore */ }
                return;
            }
            try {
                const sample = JSON.parse(evt.data);
                pushLiveSample(sample);
            } catch (e) {
                console.error('Parse error:', e);
            }
        };

        state.ws.onclose = () => {
            state.connected = false;
            updateConnectionStatus(false);
            scheduleReconnect();
        };

        state.ws.onerror = () => {
            state.ws.close();
        };
    }

    // Replay any samples that arrived while history was loading.
    function drainLiveQueue() {
        if (state.liveQueue.length === 0) return;
        const queue = state.liveQueue;
        state.liveQueue = [];
        queue.forEach(sample => pushLiveSample(sample));
    }

    function scheduleReconnect() {
        if (state.reconnectTimer) return;
        state.reconnectTimer = setTimeout(() => {
            state.reconnectTimer = null;
            connectWS();
        }, state.reconnectDelay);
        state.reconnectDelay = Math.min(state.reconnectDelay * 1.5, 30000);
    }

    function updateConnectionStatus(connected) {
        const dot = document.getElementById('connection-status');
        if (dot) {
            dot.className = 'status-dot ' + (connected ? 'connected' : 'disconnected');
            dot.title = connected ? 'Connected' : 'Disconnected';
        }
    }

    // ---- Pause/Resume ----
    function syncPauseState() {
        const shouldPause = state.pausedManual || state.pausedHover || state.pausedZoom;
        if (shouldPause !== state.paused) {
            state.paused = shouldPause;
            const btn = document.getElementById('btn-pause');
            btn.textContent = state.paused ? '▶' : '⏸';
            btn.classList.toggle('paused', state.paused);
            if (state.ws?.readyState === WebSocket.OPEN) {
                state.ws.send(JSON.stringify({ action: state.paused ? 'pause' : 'resume' }));
            }
        }
    }

    function togglePause() {
        state.pausedManual = !state.pausedManual;
        syncPauseState();
    }

    // ---- Layout Toggle ----
    function toggleLayout() {
        state.layoutMode = state.layoutMode === 'grid' ? 'list' : 'grid';
        localStorage.setItem('kula_layout', state.layoutMode);
        applyLayout();
    }

    function applyLayout() {
        const dashboard = document.getElementById('dashboard');
        const btn = document.getElementById('btn-layout');

        if (state.layoutMode === 'list') {
            dashboard.classList.add('layout-list');
            btn.classList.add('layout-active');
            btn.textContent = '⊟';
            btn.title = 'Switch to grid layout';
        } else {
            dashboard.classList.remove('layout-list');
            btn.classList.remove('layout-active');
            btn.textContent = '⊞';
            btn.title = 'Switch to list layout';
        }

        // Re-init charts for new layout
        initCharts();
        // Reload data
        if (state.lastSample) {
            // Reload history
            if (state.timeRange !== null) {
                fetchHistory(state.timeRange);
            } else if (state.customFrom && state.customTo) {
                fetchCustomHistory(state.customFrom, state.customTo);
            }
        }
    }



    // ---- Time Range ----
    function setTimeRange(seconds) {
        state.timeRange = seconds;
        state.customFrom = null;
        state.customTo = null;
        document.querySelectorAll('.time-btn[data-range]').forEach(b => b.classList.remove('active'));
        document.querySelector(`.time-btn[data-range="${seconds}"]`)?.classList.add('active');
        document.getElementById('btn-custom-range')?.classList.remove('active');

        const labels = {
            60: 'Last 1 minute', 300: 'Last 5 minutes', 900: 'Last 15 minutes', 1800: 'Last 30 minutes',
            3600: 'Last 1 hour', 10800: 'Last 3 hours', 21600: 'Last 6 hours', 43200: 'Last 12 hours',
            86400: 'Last 24 hours', 259200: 'Last 3 days', 604800: 'Last 7 days', 2592000: 'Last 30 days'
        };
        document.getElementById('time-range-display').textContent = labels[seconds] || `Last ${seconds}s`;

        resetZoomAll();
        fetchHistory(seconds);
    }

    function updateSamplingInfo(tier, resolution) {
        const el = document.getElementById('sampling-info');
        if (!el) return;
        const tierNames = ['Tier 1 (raw)', 'Tier 2 (aggregated)', 'Tier 3 (long-term)'];
        const name = tierNames[tier] || `Tier ${tier + 1}`;
        el.textContent = `${resolution} samples · ${name}`;
        state.currentResolution = resolution || '1s';
    }

    function fetchHistory(rangeSeconds) {
        if (state.loadingHistory) return;
        state.loadingHistory = true;
        document.getElementById('loading-spinner')?.classList.remove('hidden');

        const to = new Date().toISOString();
        const from = new Date(Date.now() - rangeSeconds * 1000).toISOString();
        fetch(`/api/history?from=${from}&to=${to}`)
            .then(r => r.json())
            .then(response => {
                const data = response.samples || response;
                const isEnvelope = response.samples !== undefined;

                if (isEnvelope) {
                    updateSamplingInfo(response.tier, response.resolution);
                }

                if (!Array.isArray(data) || data.length === 0) {
                    clearAllChartData();
                    state.dataBuffer = [];
                    setChartTimeRange();
                    updateAllCharts();
                    state.loadingHistory = false;
                    document.getElementById('loading-spinner')?.classList.add('hidden');
                    return;
                }

                // Clear all chart data before loading history
                clearAllChartData();
                state.dataBuffer = [];

                // Batch add all historical points WITHOUT chart.update() per sample
                const processed = insertGapsInHistory(data, response?.resolution);
                processed.forEach(item => {
                    if (item._gap) {
                        addGapToCharts(new Date(item.ts));
                        return;
                    }
                    const sample = item.data || item;
                    const ts = new Date(sample.ts);
                    state.dataBuffer.push(sample);
                    addSampleToCharts(sample, ts);
                });

                // Trim buffer
                if (state.dataBuffer.length > state.maxBufferSize) {
                    state.dataBuffer = state.dataBuffer.slice(-state.maxBufferSize);
                }

                // Single batch update of all charts
                trimChartsToTimeRange();
                updateAllCharts();

                // Update gauges/header with latest sample
                const lastSample = data[data.length - 1];
                const s = lastSample.data || lastSample;
                state.lastSample = s;
                updateGauges(s);
                updateHeader(s);
                updateSubtitles(s);
                evaluateAlerts(s);

                state.loadingHistory = false;
                document.getElementById('loading-spinner')?.classList.add('hidden');
                drainLiveQueue();
            })
            .catch(e => {
                console.error('History fetch error:', e);
                state.loadingHistory = false;
                document.getElementById('loading-spinner')?.classList.add('hidden');
                drainLiveQueue();
            });
    }

    function fetchCustomHistory(fromDate, toDate) {
        if (state.loadingHistory) return;
        state.loadingHistory = true;
        document.getElementById('loading-spinner')?.classList.remove('hidden');

        const from = fromDate.toISOString();
        const to = toDate.toISOString();
        fetch(`/api/history?from=${from}&to=${to}`)
            .then(r => r.json())
            .then(response => {
                const data = response.samples || response;
                const isEnvelope = response.samples !== undefined;

                if (isEnvelope) {
                    updateSamplingInfo(response.tier, response.resolution);
                }

                clearAllChartData();
                state.dataBuffer = [];

                if (Array.isArray(data) && data.length > 0) {
                    const processed = insertGapsInHistory(data, response?.resolution);
                    processed.forEach(item => {
                        if (item._gap) {
                            addGapToCharts(new Date(item.ts));
                            return;
                        }
                        const sample = item.data || item;
                        const ts = new Date(sample.ts);
                        state.dataBuffer.push(sample);
                        addSampleToCharts(sample, ts);
                    });

                    if (state.dataBuffer.length > state.maxBufferSize) {
                        state.dataBuffer = state.dataBuffer.slice(-state.maxBufferSize);
                    }

                    const lastSample = data[data.length - 1];
                    const s = lastSample.data || lastSample;
                    state.lastSample = s;
                    updateGauges(s);
                    updateHeader(s);
                    updateSubtitles(s);
                    evaluateAlerts(s);
                }

                setChartTimeRange();
                updateAllCharts();
                state.loadingHistory = false;
                document.getElementById('loading-spinner')?.classList.add('hidden');
                drainLiveQueue();
            })
            .catch(e => {
                console.error('Custom history fetch error:', e);
                state.loadingHistory = false;
                document.getElementById('loading-spinner')?.classList.add('hidden');
                drainLiveQueue();
            });
    }

    // ---- Custom Time Range ----
    function toggleCustomTimePicker() {
        const customEl = document.getElementById('time-custom');
        const isHidden = customEl.classList.contains('hidden');
        if (isHidden) {
            customEl.classList.remove('hidden');
            document.getElementById('btn-custom-range').classList.add('active');
            // Set default values
            const now = new Date();
            const from = new Date(now.getTime() - 3600000); // 1 hour ago
            document.getElementById('custom-from').value = toLocalISOString(from);
            document.getElementById('custom-to').value = toLocalISOString(now);
        } else {
            customEl.classList.add('hidden');
            document.getElementById('btn-custom-range').classList.remove('active');
        }
    }

    function applyCustomRange() {
        const fromVal = document.getElementById('custom-from').value;
        const toVal = document.getElementById('custom-to').value;
        if (!fromVal || !toVal) return;

        const fromDate = new Date(fromVal);
        const toDate = new Date(toVal);
        if (fromDate >= toDate) return;

        state.timeRange = null;
        state.customFrom = fromDate;
        state.customTo = toDate;

        // Deselect preset buttons
        document.querySelectorAll('.time-btn[data-range]').forEach(b => b.classList.remove('active'));

        const fmt = d => d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
        document.getElementById('time-range-display').textContent = `${fmt(fromDate)} → ${fmt(toDate)}`;

        resetZoomAll();
        fetchCustomHistory(fromDate, toDate);
    }

    function toLocalISOString(date) {
        const pad = n => String(n).padStart(2, '0');
        return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
    }

    // ---- Auth ----
    function checkAuth() {
        fetch('/api/auth/status')
            .then(r => r.json())
            .then(data => {
                if (data.auth_required && !data.authenticated) {
                    document.getElementById('login-overlay').classList.remove('hidden');
                    document.getElementById('dashboard').style.filter = 'blur(8px)';
                } else {
                    document.getElementById('login-overlay').classList.add('hidden');
                    document.getElementById('dashboard').style.filter = '';
                    connectWS();
                }
            })
            .catch(() => connectWS()); // If auth check fails, try connecting anyway
    }

    function handleLogin(e) {
        e.preventDefault();
        const user = document.getElementById('login-user').value;
        const pass = document.getElementById('login-pass').value;
        const errorEl = document.getElementById('login-error');

        fetch('/api/login', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ username: user, password: pass }),
        })
            .then(r => {
                if (!r.ok) throw new Error('Invalid credentials');
                return r.json();
            })
            .then(() => {
                document.getElementById('login-overlay').classList.add('hidden');
                document.getElementById('dashboard').style.filter = '';
                errorEl.classList.add('hidden');
                connectWS();
            })
            .catch(err => {
                errorEl.textContent = err.message;
                errorEl.classList.remove('hidden');
            });
    }

    // ---- Utility ----
    function formatBytesShort(bytes) {
        if (bytes === 0 || bytes === undefined || bytes === null || isNaN(bytes)) return '0 B';
        if (Math.abs(bytes) < 1) return '0 B';
        const units = ['B', 'KB', 'MB', 'GB', 'TB'];
        const i = Math.floor(Math.log(Math.abs(bytes)) / Math.log(1024));
        const idx = Math.max(0, Math.min(i, units.length - 1));
        return (bytes / Math.pow(1024, idx)).toFixed(idx > 0 ? 1 : 0) + ' ' + units[idx];
    }

    function formatMbps(v) {
        if (v < 1) return (v * 1000).toFixed(0) + ' Kbps';
        return v.toFixed(2) + ' Mbps';
    }

    function formatPPS(v) {
        if (v === undefined || v === null || isNaN(v)) return '0 pps';
        if (v >= 1000000) return (v / 1000000).toFixed(1) + ' Mpps';
        if (v >= 1000) return (v / 1000).toFixed(1) + ' Kpps';
        return Math.round(v) + ' pps';
    }

    // ---- Gap Insertion ----
    function insertGapsInHistory(data, resolutionStr = '1s') {
        if (state.joinMetrics || data.length < 2) return data;

        let expectedInterval = 1000; // default 1s
        if (typeof resolutionStr === 'string') {
            const num = parseInt(resolutionStr) || 1;
            if (resolutionStr.endsWith('s')) expectedInterval = num * 1000;
            else if (resolutionStr.endsWith('m')) expectedInterval = num * 60000;
            else if (resolutionStr.endsWith('h')) expectedInterval = num * 3600000;
        }

        const gapThreshold = expectedInterval * 2.5;

        const result = [];
        for (let i = 0; i < data.length; i++) {
            result.push(data[i]);
            if (i < data.length - 1) {
                const curTs = new Date(data[i].ts || data[i].data?.ts).getTime();
                const nextTs = new Date(data[i + 1].ts || data[i + 1].data?.ts).getTime();
                if (nextTs - curTs > gapThreshold) {
                    // Insert a null gap marker
                    result.push({ _gap: true, ts: new Date(curTs + expectedInterval).toISOString() });
                }
            }
        }
        return result;
    }

    function addGapToCharts(ts) {
        Object.values(state.charts).forEach(chart => {
            if (!chart?.data?.datasets) return;
            chart.data.datasets.forEach(ds => {
                if (Array.isArray(ds.data)) ds.data.push({ x: ts, y: null });
            });
        });
    }

    // ---- Hover Pause ----
    function setupHoverPause() {
        document.querySelectorAll('.chart-card').forEach(card => {
            card.addEventListener('mouseenter', () => {
                if (!state.pausedHover) {
                    state.pausedHover = true;
                    syncPauseState();
                }
            });
            card.addEventListener('mouseleave', () => {
                if (state.pausedHover) {
                    state.pausedHover = false;
                    syncPauseState();
                }
            });
        });
    }

    // ---- Focus Mode ----
    const chartCardIds = [
        'card-cpu', 'card-loadavg', 'card-memory', 'card-swap',
        'card-network', 'card-pps', 'card-connections',
        'card-disk-io', 'card-disk-util', 'card-disk-space',
        'card-processes', 'card-entropy', 'card-self'
    ];

    function toggleFocusMode() {
        const grid = document.getElementById('charts-grid');
        const btn = document.getElementById('btn-focus');

        if (state.focusMode && !state.focusSelecting) {
            // Exit focus mode
            state.focusMode = false;
            grid.classList.remove('focus-active', 'focus-selecting');
            btn.classList.remove('focus-active');
            chartCardIds.forEach(id => {
                const el = document.getElementById(id);
                if (el) el.classList.remove('focus-visible', 'focus-selected');
            });
            removeFocusBar();
            localStorage.removeItem('kula_focus_visible');
            state.focusVisible = null;
            return;
        }

        if (state.focusSelecting) {
            // Apply selection
            const selected = [];
            chartCardIds.forEach(id => {
                const el = document.getElementById(id);
                if (el?.classList.contains('focus-selected')) selected.push(id);
            });

            if (selected.length === 0) {
                // No selection = exit
                state.focusMode = false;
                state.focusSelecting = false;
                grid.classList.remove('focus-active', 'focus-selecting');
                btn.classList.remove('focus-active');
                removeFocusBar();
                return;
            }

            state.focusVisible = selected;
            localStorage.setItem('kula_focus_visible', JSON.stringify(selected));
            state.focusSelecting = false;
            grid.classList.remove('focus-selecting');
            grid.classList.add('focus-active');
            chartCardIds.forEach(id => {
                const el = document.getElementById(id);
                if (el) {
                    el.classList.toggle('focus-visible', selected.includes(id));
                    el.classList.remove('focus-selected');
                }
            });
            removeFocusBar();
            return;
        }

        // Enter selection mode
        state.focusMode = true;
        state.focusSelecting = true;
        grid.classList.add('focus-selecting');
        grid.classList.remove('focus-active');
        btn.classList.add('focus-active');

        // Pre-select previously visible cards
        chartCardIds.forEach(id => {
            const el = document.getElementById(id);
            if (el) {
                if (state.focusVisible?.includes(id)) {
                    el.classList.add('focus-selected');
                } else {
                    el.classList.remove('focus-selected');
                }
            }
        });

        showFocusBar();

        // Click handler for selection
        chartCardIds.forEach(id => {
            const el = document.getElementById(id);
            if (el) {
                el._focusClick = () => el.classList.toggle('focus-selected');
                el.addEventListener('click', el._focusClick);
            }
        });
    }

    function showFocusBar() {
        removeFocusBar();
        const bar = document.createElement('div');
        bar.className = 'focus-bar';
        bar.id = 'focus-bar';
        bar.innerHTML = '<span>Select graphs to display, then click Done</span><button id="btn-focus-done">Done</button><button id="btn-focus-cancel">Cancel</button>';
        const grid = document.getElementById('charts-grid');
        grid.parentNode.insertBefore(bar, grid);
        document.getElementById('btn-focus-done').addEventListener('click', toggleFocusMode);
        document.getElementById('btn-focus-cancel').addEventListener('click', () => {
            state.focusSelecting = false;
            state.focusMode = false;
            const g = document.getElementById('charts-grid');
            g.classList.remove('focus-selecting', 'focus-active');
            document.getElementById('btn-focus').classList.remove('focus-active');
            chartCardIds.forEach(id => {
                const el = document.getElementById(id);
                if (el) { el.classList.remove('focus-selected'); if (el._focusClick) el.removeEventListener('click', el._focusClick); }
            });
            removeFocusBar();
        });
    }

    function removeFocusBar() {
        const bar = document.getElementById('focus-bar');
        if (bar) bar.remove();
        chartCardIds.forEach(id => {
            const el = document.getElementById(id);
            if (el?._focusClick) { el.removeEventListener('click', el._focusClick); delete el._focusClick; }
        });
    }

    function applyStoredFocusMode() {
        if (state.focusVisible && state.focusVisible.length > 0) {
            state.focusMode = true;
            const grid = document.getElementById('charts-grid');
            grid.classList.add('focus-active');
            document.getElementById('btn-focus').classList.add('focus-active');
            chartCardIds.forEach(id => {
                const el = document.getElementById(id);
                if (el) el.classList.toggle('focus-visible', state.focusVisible.includes(id));
            });
        }
    }

    // ---- Init ----
    function init() {
        // Fetch server config for join_metrics
        fetch('/api/config')
            .then(r => r.json())
            .then(cfg => {
                if (cfg.join_metrics !== undefined) state.joinMetrics = cfg.join_metrics;
                if (cfg.version) {
                    const versionEl = document.getElementById('kula-version');
                    if (versionEl) versionEl.textContent = 'v' + cfg.version;
                }
                if (cfg.os) {
                    const osEl = document.getElementById('sys-os');
                    if (osEl) osEl.textContent = cfg.os;
                }
                if (cfg.kernel) {
                    const kernelEl = document.getElementById('sys-kernel');
                    if (kernelEl) kernelEl.textContent = cfg.kernel;
                }
                if (cfg.arch) {
                    const archEl = document.getElementById('sys-arch');
                    if (archEl) archEl.textContent = cfg.arch;
                }
                if (cfg.hostname) {
                    const hostnameEl = document.getElementById('hostname');
                    if (hostnameEl) hostnameEl.textContent = cfg.hostname;
                    document.title = `KULA - ${cfg.hostname}`;
                }
            })
            .catch(() => { });

        // Apply stored layout
        applyLayout();

        // Apply stored focus mode
        applyStoredFocusMode();

        // Event listeners
        document.getElementById('btn-pause').addEventListener('click', togglePause);
        document.getElementById('btn-layout').addEventListener('click', toggleLayout);
        document.getElementById('btn-alerts').addEventListener('click', toggleAlertDropdown);
        document.getElementById('btn-info').addEventListener('click', toggleInfoDropdown);
        document.getElementById('btn-time-menu').addEventListener('click', (e) => {
            e.stopPropagation();
            const list = document.getElementById('time-presets-list');
            list.classList.toggle('open');
            state.timeDropdownOpen = list.classList.contains('open');
        });
        document.getElementById('btn-focus').addEventListener('click', toggleFocusMode);
        document.getElementById('login-form').addEventListener('submit', handleLogin);
        document.getElementById('btn-custom-range').addEventListener('click', toggleCustomTimePicker);
        document.getElementById('btn-apply-custom').addEventListener('click', applyCustomRange);

        document.querySelectorAll('.time-btn[data-range]').forEach(btn => {
            btn.addEventListener('click', () => {
                setTimeRange(parseInt(btn.dataset.range));
                if (state.timeDropdownOpen) {
                    state.timeDropdownOpen = false;
                    document.getElementById('time-presets-list').classList.remove('open');
                }
            });
        });

        // Double-click on any chart to reset zoom
        document.querySelectorAll('.chart-body canvas').forEach(canvas => {
            canvas.addEventListener('dblclick', resetZoomAll);
        });

        // Hover-pause on chart cards
        setupHoverPause();

        // Close dropdowns when clicking outside
        document.addEventListener('click', (e) => {
            if (state.alertDropdownOpen && !e.target.closest('#alert-container')) {
                state.alertDropdownOpen = false;
                document.getElementById('alert-dropdown').classList.add('hidden');
            }
            if (state.infoDropdownOpen && !e.target.closest('#info-container')) {
                state.infoDropdownOpen = false;
                document.getElementById('info-dropdown').classList.add('hidden');
            }
            if (state.timeDropdownOpen && !e.target.closest('.time-presets')) {
                state.timeDropdownOpen = false;
                document.getElementById('time-presets-list').classList.remove('open');
            }
        });

        checkAuth();
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', init);
    } else {
        init();
    }
})();
