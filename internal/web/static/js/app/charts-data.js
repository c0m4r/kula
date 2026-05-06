/* ============================================================
   charts-data.js — Chart data ingestion, updates, zoom sync,
   gap insertion, device selectors, and the live sample pipeline.
   ============================================================ */
'use strict';
import { state, colors } from './state.js';
import { formatBytesShort } from './utils.js';
import { apiUrl } from './url.js';
import { createTimeSeriesChart, setChartTimeRange, updateChartLabels } from './charts-init.js';
import { updateHeader, updateSubtitles } from './header.js';
import { updateGauges } from './gauges.js';
import { evaluateAlerts } from './alerts.js';
import { applyStoredFocusMode } from './focus-mode.js';
import { addSampleToSplitCharts, updateSplitSelectors } from './split.js';

// CSS order values for dynamic app chart grouping within the grid.
const APP_ORDER_NGINX = 10;
const APP_ORDER_APACHE2 = 15;
const APP_ORDER_CONTAINERS = 20;
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

// ---- Data Update ----
export function addSampleToCharts(item, ts) {
    let s = item.data || item;
    if (state.currentAggregation === 'min' && item.min) s = item.min;
    if (state.currentAggregation === 'max' && item.max) s = item.max;

    const point = (v) => ({ x: ts, y: v });

    // CPU
    if (state.charts.cpu && s.cpu?.total) {
        state.charts.cpu.data.datasets[0].data.push(point(s.cpu.total.user));
        state.charts.cpu.data.datasets[1].data.push(point(s.cpu.total.system));
        state.charts.cpu.data.datasets[2].data.push(point(s.cpu.total.iowait));
        state.charts.cpu.data.datasets[3].data.push(point(s.cpu.total.steal));
        state.charts.cpu.data.datasets[4].data.push(point(s.cpu.total.usage));
    }

    // CPU Temperature
    const tempCard = document.getElementById('card-cpu-temp');
    if (state.charts.cputemp && ((s.cpu?.sensors && s.cpu.sensors.length > 0) || s.cpu?.temp > 0)) {
        if (tempCard) {
            tempCard.classList.remove('hidden');
            document.getElementById('thermals-title')?.classList.remove('hidden');
            document.getElementById('thermals-grid')?.classList.remove('hidden');
        }

        const hasSensors = s.cpu?.sensors && s.cpu.sensors.length > 0;

        if (hasSensors) {
            const incomingNames = s.cpu.sensors.map(sens => sens.name);
            if (incomingNames.join(',') !== state.cpuTempSensorNames.join(',')) {
                state.cpuTempSensorNames = incomingNames;
                const cpuTempColorPairs = [
                    [colors.orange, colors.orangeAlpha],
                    [colors.red, colors.redAlpha],
                    [colors.yellow, colors.yellowAlpha],
                    [colors.pink, colors.pinkAlpha],
                    [colors.purple, colors.purpleAlpha],
                    [colors.cyan, colors.cyanAlpha],
                ];
                state.charts.cputemp.data.datasets = incomingNames.map((name, i) => ({
                    label: name,
                    borderColor: cpuTempColorPairs[i % cpuTempColorPairs.length][0],
                    backgroundColor: cpuTempColorPairs[i % cpuTempColorPairs.length][1],
                    fill: i === 0, // only fill the primary one
                    data: [],
                    pointHitRadius: 5,
                }));
            }

            s.cpu.sensors.forEach((sens, i) => {
                if (i < state.charts.cputemp.data.datasets.length) {
                    state.charts.cputemp.data.datasets[i].data.push(point(sens.value));
                }
            });
        } else {
            // Fallback to plain Temperature if no sensors array
            if (state.charts.cputemp.data.datasets.length !== 1 || state.charts.cputemp.data.datasets[0].label !== 'Temperature') {
                state.cpuTempSensorNames = [];
                state.charts.cputemp.data.datasets = [
                    { label: 'Temperature', borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true, data: [] },
                ];
            }
            state.charts.cputemp.data.datasets[0].data.push(point(s.cpu.temp));
        }
    }

    // Load Average
    if (state.charts.loadavg && s.lavg) {
        state.charts.loadavg.data.datasets[0].data.push(point(s.lavg.load1));
        state.charts.loadavg.data.datasets[1].data.push(point(s.lavg.load5));
        state.charts.loadavg.data.datasets[2].data.push(point(s.lavg.load15));
    }

    // Memory — with Free, Available, and Shmem
    if (state.charts.memory && s.mem) {
        state.charts.memory.data.datasets[0].data.push(point(s.mem.used));
        state.charts.memory.data.datasets[1].data.push(point(s.mem.buffers));
        state.charts.memory.data.datasets[2].data.push(point(s.mem.cached));
        state.charts.memory.data.datasets[3].data.push(point(s.mem.shmem || 0));
        state.charts.memory.data.datasets[4].data.push(point(s.mem.free));
        state.charts.memory.data.datasets[5].data.push(point(s.mem.available));
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

    // Network (selected non-lo interface) — skip when split is active
    if (!state.splitNet && state.charts.network && s.net?.ifaces) {
        let rx = 0, tx = 0;
        const iface = s.net.ifaces.find(i => i.name === state.selectedNet);
        if (iface) {
            rx = iface.rx_mbps || 0;
            tx = iface.tx_mbps || 0;
        } else if (!state.selectedNet && s.net.ifaces.length > 0) {
            // Sum all if nothing selected
            s.net.ifaces.forEach(i => { if (i.name !== 'lo') { rx += i.rx_mbps || 0; tx += i.tx_mbps || 0; } });
        }
        state.charts.network.data.datasets[0].data.push(point(rx));
        state.charts.network.data.datasets[1].data.push(point(tx));
    }

    // Packets per second (selected non-lo interface) — skip when split is active
    if (!state.splitNet && state.charts.pps && s.net?.ifaces) {
        let rxPps = 0, txPps = 0;
        const iface = s.net.ifaces.find(i => i.name === state.selectedNet);
        if (iface) {
            rxPps = iface.rx_pps || 0;
            txPps = iface.tx_pps || 0;
        } else if (!state.selectedNet && s.net.ifaces.length > 0) {
            s.net.ifaces.forEach(i => { if (i.name !== 'lo') { rxPps += i.rx_pps || 0; txPps += i.tx_pps || 0; } });
        }
        state.charts.pps.data.datasets[0].data.push(point(rxPps));
        state.charts.pps.data.datasets[1].data.push(point(txPps));
    }

    // Connections
    if (state.charts.connections && s.net?.sockets) {
        state.charts.connections.data.datasets[0].data.push(point(s.net.sockets.tcp_inuse));
        state.charts.connections.data.datasets[1].data.push(point(s.net.sockets.udp_inuse));
        state.charts.connections.data.datasets[2].data.push(point(s.net.sockets.tcp_tw));
        state.charts.connections.data.datasets[3].data.push(point(s.net?.tcp?.curr_estab || 0));
        state.charts.connections.data.datasets[4].data.push(point(s.net?.tcp?.in_errs_ps || 0));
        state.charts.connections.data.datasets[5].data.push(point(s.net?.tcp?.out_rsts_ps || 0));
        state.charts.connections.data.datasets[6].data.push(point(s.net?.tcp?.retrans_ps || 0));
    }

    // Disk I/O (selected device) — skip when split is active
    if (!state.splitDiskIo && state.charts.diskio && s.disk?.devices) {
        let rBps = 0, wBps = 0, rIops = 0, wIops = 0;
        const d = s.disk.devices.find(d => d.name === state.selectedDiskIo);
        if (d) {
            rBps = d.read_bps || 0;
            wBps = d.write_bps || 0;
            rIops = d.reads_ps || 0;
            wIops = d.writes_ps || 0;
        } else if (!state.selectedDiskIo && s.disk.devices.length > 0) {
            s.disk.devices.forEach(d => {
                rBps += d.read_bps || 0;
                wBps += d.write_bps || 0;
                rIops += d.reads_ps || 0;
                wIops += d.writes_ps || 0;
            });
        }
        state.charts.diskio.data.datasets[0].data.push(point(rBps));
        state.charts.diskio.data.datasets[1].data.push(point(wBps));
        state.charts.diskio.data.datasets[2].data.push(point(rIops));
        state.charts.diskio.data.datasets[3].data.push(point(wIops));
    }

    // Disk Temperature — skip when split is active
    const diskTempCard = document.getElementById('card-disk-temp');
    if (!state.splitDiskTemp && state.charts.disktemp && s.disk?.devices) {
        const d = s.disk.devices.find(d => d.name === state.selectedDiskTemp);
        const hasSensors = d && d.sensors && d.sensors.length > 0;
        const hasTemp = d && d.temp > 0;

        if (hasSensors || hasTemp) {
            if (diskTempCard) {
                diskTempCard.classList.remove('hidden');
                document.getElementById('thermals-title')?.classList.remove('hidden');
                document.getElementById('thermals-grid')?.classList.remove('hidden');
            }

            if (hasSensors) {
                const incomingNames = d.sensors.map(sens => sens.name);
                if (incomingNames.join(',') !== state.diskTempSensorNames.join(',')) {
                    state.diskTempSensorNames = incomingNames;
                    const tempColorPairs = [
                        [colors.red, colors.redAlpha],
                        [colors.orange, colors.orangeAlpha],
                        [colors.yellow, colors.yellowAlpha],
                        [colors.pink, colors.pinkAlpha],
                        [colors.purple, colors.purpleAlpha],
                        [colors.cyan, colors.cyanAlpha],
                    ];
                    state.charts.disktemp.data.datasets = incomingNames.map((name, i) => ({
                        label: name,
                        borderColor: tempColorPairs[i % tempColorPairs.length][0],
                        backgroundColor: tempColorPairs[i % tempColorPairs.length][1],
                        fill: i === 0,
                        data: [],
                        pointHitRadius: 5,
                    }));
                }

                d.sensors.forEach((sens, i) => {
                    if (i < state.charts.disktemp.data.datasets.length) {
                        state.charts.disktemp.data.datasets[i].data.push(point(sens.value));
                    }
                });
            } else {
                if (state.charts.disktemp.data.datasets.length !== 1 || state.charts.disktemp.data.datasets[0].label !== 'Temperature') {
                    state.diskTempSensorNames = [];
                    state.charts.disktemp.data.datasets = [
                        { label: 'Temperature', borderColor: colors.red, backgroundColor: colors.redAlpha, fill: true, data: [] },
                    ];
                }
                state.charts.disktemp.data.datasets[0].data.push(point(d.temp));
            }
        } else {
            if (diskTempCard) diskTempCard.classList.add('hidden');
        }
    }

    // Disk Space — single dataset for selected mount — skip when split is active
    if (!state.splitDiskSpace && state.charts.diskspace && s.disk?.filesystems && s.disk.filesystems.length > 0) {
        if (state.charts.diskspace.data.datasets.length !== 1 || state.charts.diskspace.data.datasets[0].label !== 'Space Used %') {
            state.charts.diskspace.data.datasets = [{
                label: 'Space Used %',
                borderColor: colors.purple,
                backgroundColor: colors.purpleAlpha,
                fill: true,
                data: [],
                pointHitRadius: 5,
            }];
        }
        let usedPct = 0, used = 0, total = 0;
        const fs = s.disk.filesystems.find(f => f.mount === state.selectedDiskSpace);
        if (fs) {
            usedPct = fs.used_pct || 0;
            used = fs.used || 0;
            total = fs.total || 0;
        } else if (!state.selectedDiskSpace) {
            s.disk.filesystems.forEach(f => { used += f.used || 0; total += f.total || 0; });
            if (total > 0) usedPct = (used / total) * 100;
        }
        state.charts.diskspace.data.datasets[0].data.push({ x: ts, y: usedPct, used, total });
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
        state.charts.self.data.datasets[1].data.push(point(s.self.mem_rss || 0));
    }

    // GPU Metrics — skip regular cards when split is active
    if (!state.splitGpu && s.gpu && s.gpu.length > 0) {
        const g = s.gpu.find(g => g.name === state.selectedGpuLoad) || s.gpu[0];
        const hasAnyGpuMetric = (g.load_pct > 0 || g.power_w > 0 || g.vram_total > 0 || g.temp > 0);

        if (hasAnyGpuMetric) {
            if (state.charts.gpuload && (g.load_pct > 0 || g.power_w > 0)) {
                document.getElementById('card-gpu-load')?.classList.remove('hidden');
                state.charts.gpuload.data.datasets[0].data.push(point(g.load_pct || 0));
                state.charts.gpuload.data.datasets[1].data.push(point(g.power_w || 0));
            } else {
                document.getElementById('card-gpu-load')?.classList.add('hidden');
            }
            if (state.charts.vram && g.vram_total > 0 && g.vram_used > 0) {
                document.getElementById('card-vram')?.classList.remove('hidden');
                state.charts.vram.data.datasets[0].data.push(point(g.vram_used || 0));
                state.charts.vram.options.scales.y.max = g.vram_total > 0 ? g.vram_total : undefined;
            } else {
                document.getElementById('card-vram')?.classList.add('hidden');
            }
            if (state.charts.gputemp && g.temp > 0) {
                document.getElementById('card-gpu-temp')?.classList.remove('hidden');
                document.getElementById('thermals-title')?.classList.remove('hidden');
                document.getElementById('thermals-grid')?.classList.remove('hidden');
                state.charts.gputemp.data.datasets[0].data.push(point(g.temp));
            } else {
                document.getElementById('card-gpu-temp')?.classList.add('hidden');
            }
        } else {
            document.getElementById('card-gpu-load')?.classList.add('hidden');
            document.getElementById('card-vram')?.classList.add('hidden');
            document.getElementById('card-gpu-temp')?.classList.add('hidden');
        }

        // If in focus mode, re-apply visibility based on the new .hidden state
        if (state.focusMode && typeof applyStoredFocusMode === 'function') {
            applyStoredFocusMode();
        }
    } else if (!state.splitGpu) {
        document.getElementById('card-gpu-load')?.classList.add('hidden');
        document.getElementById('card-vram')?.classList.add('hidden');
        document.getElementById('card-gpu-temp')?.classList.add('hidden');
        if (state.focusMode && typeof applyStoredFocusMode === 'function') {
            applyStoredFocusMode();
        }
    }

    // ---- Power Supply (batteries/UPS) — dynamic charts in system metrics grid ----
    if (s.psu && s.psu.length > 0) {
        for (const ps of s.psu) {
            // Only chart batteries and UPS, skip Mains adapters
            if (ps.type !== 'Battery' && ps.type !== 'UPS') continue;

            const psuKey = `psu_${ps.name}`;
            if (!state.psuCharts) state.psuCharts = {};

            if (!state.psuCharts[psuKey]) {
                const grid = document.getElementById('charts-grid');
                if (grid) {
                    const wrapper = document.createElement('div');
                    wrapper.className = 'chart-card';
                    wrapper.id = `card-${psuKey}`;
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

                    state.psuCharts[psuKey] = createTimeSeriesChart(`chart-${psuKey}`, [
                        { label: 'Capacity %', borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
                        { label: 'Power W', borderColor: colors.orange, data: [], fill: false },
                    ], { beginAtZero: true, max: 100, ticks: { callback: v => v + '%' } });

                    // Add second y-axis for power
                    if (state.psuCharts[psuKey]) {
                        state.psuCharts[psuKey].data.datasets[1].yAxisID = 'y1';
                        state.psuCharts[psuKey].options.scales.y1 = {
                            position: 'right',
                            beginAtZero: true,
                            grid: { display: false },
                            ticks: { callback: v => v.toFixed(1) + ' W' },
                        };
                        state.psuCharts[psuKey].update('none');
                    }
                }
            }

            const chart = state.psuCharts[psuKey];
            if (chart) {
                chart.data.datasets[0].data.push(point(ps.capacity || 0));
                chart.data.datasets[1].data.push(point(ps.power_w || 0));
                if (!state.loadingHistory) chart.update('none');
            }

            const sub = document.getElementById(`${psuKey}-subtitle`);
            if (sub) {
                const parts = [`${ps.capacity}%`];
                if (ps.status) parts.push(ps.status);
                if (ps.power_w > 0) parts.push(`${ps.power_w.toFixed(1)} W`);
                if (ps.voltage_v > 0) parts.push(`${ps.voltage_v.toFixed(2)} V`);
                sub.textContent = parts.join('  ');
            }
        }
    }

    // ---- Applications (all charts created dynamically) ----
    let appsVisible = false;
    const seenContainers = new Set();
    const seenCustom = new Set();
    const colorList = [colors.blue, colors.green, colors.orange, colors.purple, colors.cyan, colors.red, colors.yellow, colors.pink, colors.teal, colors.lime];

    // Nginx — create charts on first data, push data, update subtitles
    if (s.apps?.nginx) {
        const n = s.apps.nginx;
        appsVisible = true;

        if (!state.charts.nginxConn) {
            createAppChartCard('card-nginx-connections', 'chart-nginx-connections', 'nginx-conn-subtitle', 'Nginx \u2014 Connections', APP_ORDER_NGINX);
            state.charts.nginxConn = createTimeSeriesChart('chart-nginx-connections', [
                { label: 'Active Connections', borderColor: colors.blue, backgroundColor: colors.blueAlpha, fill: true, data: [] },
            ]);
        }
        if (state.charts.nginxConn) {
            state.charts.nginxConn.data.datasets[0].data.push(point(n.active_conn));
            const sub = document.getElementById('nginx-conn-subtitle');
            if (sub) sub.textContent = `Active: ${n.active_conn}`;
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
            state.charts.nginxReqs.data.datasets[0].data.push(point(n.accepts_ps));
            state.charts.nginxReqs.data.datasets[1].data.push(point(n.handled_ps));
            state.charts.nginxReqs.data.datasets[2].data.push(point(n.requests_ps));
            const sub = document.getElementById('nginx-reqs-subtitle');
            if (sub) sub.textContent = `Req/s: ${n.requests_ps?.toFixed(1) || '0'}`;
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
            state.charts.nginxRw.data.datasets[0].data.push(point(n.reading));
            state.charts.nginxRw.data.datasets[1].data.push(point(n.writing));
            state.charts.nginxRw.data.datasets[2].data.push(point(n.waiting));
            const sub = document.getElementById('nginx-rw-subtitle');
            if (sub) sub.textContent = `R: ${n.reading}  W: ${n.writing}  Wait: ${n.waiting}`;
        }
    }

    // Apache2 — create charts on first data, push data, update subtitles
    if (s.apps?.apache2) {
        const a = s.apps.apache2;
        appsVisible = true;

        if (!state.charts.apache2Workers) {
            createAppChartCard('card-apache2-workers', 'chart-apache2-workers', 'apache2-workers-subtitle', 'Apache2 \u2014 Workers', APP_ORDER_APACHE2);
            state.charts.apache2Workers = createTimeSeriesChart('chart-apache2-workers', [
                { label: 'Busy', borderColor: colors.orange, backgroundColor: colors.orangeAlpha, fill: true, data: [] },
                { label: 'Idle', borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
            ]);
        }
        if (state.charts.apache2Workers) {
            state.charts.apache2Workers.data.datasets[0].data.push(point(a.busy_workers));
            state.charts.apache2Workers.data.datasets[1].data.push(point(a.idle_workers));
            const sub = document.getElementById('apache2-workers-subtitle');
            if (sub) sub.textContent = `Busy: ${a.busy_workers}  Idle: ${a.idle_workers}`;
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
            state.charts.apache2Tput.data.datasets[0].data.push(point(a.accesses_ps));
            state.charts.apache2Tput.data.datasets[1].data.push(point(a.req_per_sec));
            state.charts.apache2Tput.data.datasets[2].data.push(point(a.kbytes_ps));
            const sub = document.getElementById('apache2-tput-subtitle');
            if (sub) sub.textContent = `Req/s: ${a.req_per_sec?.toFixed(1) || '0'}  kB/s: ${a.kbytes_ps?.toFixed(1) || '0'}`;
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
            state.charts.apache2States.data.datasets[0].data.push(point(a.waiting));
            state.charts.apache2States.data.datasets[1].data.push(point(a.reading));
            state.charts.apache2States.data.datasets[2].data.push(point(a.sending));
            state.charts.apache2States.data.datasets[3].data.push(point(a.keepalive));
            state.charts.apache2States.data.datasets[4].data.push(point(a.starting));
            state.charts.apache2States.data.datasets[5].data.push(point(a.dns));
            state.charts.apache2States.data.datasets[6].data.push(point(a.closing));
            state.charts.apache2States.data.datasets[7].data.push(point(a.logging));
            state.charts.apache2States.data.datasets[8].data.push(point(a.graceful));
            state.charts.apache2States.data.datasets[9].data.push(point(a.idle_cleanup));
            state.charts.apache2States.data.datasets[10].data.push(point(a.open_slots));
            const sub = document.getElementById('apache2-states-subtitle');
            if (sub) sub.textContent = `Busy: ${a.busy_workers}  Idle: ${a.idle_workers}  Slots: ${a.open_slots}`;
        }
    }

    // MySQL — create charts on first data
    if (s.apps?.mysql) {
        const m = s.apps.mysql;
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
            state.charts.mysqlConn.data.datasets[0].data.push(point(m.threads_connected));
            state.charts.mysqlConn.data.datasets[1].data.push(point(m.threads_running));
            state.charts.mysqlConn.data.datasets[2].data.push(point(m.threads_cached));
            state.charts.mysqlConn.data.datasets[3].data.push(point(m.max_conns));
            const sub = document.getElementById('mysql-conn-subtitle');
            if (sub) sub.textContent = `Connected: ${m.threads_connected}  Running: ${m.threads_running}  Cached: ${m.threads_cached}  Max: ${m.max_conns}`;
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
            state.charts.mysqlQPS.data.datasets[0].data.push(point(m.queries_ps));
            state.charts.mysqlQPS.data.datasets[1].data.push(point(m.select_ps));
            state.charts.mysqlQPS.data.datasets[2].data.push(point(m.insert_ps));
            state.charts.mysqlQPS.data.datasets[3].data.push(point(m.update_ps));
            state.charts.mysqlQPS.data.datasets[4].data.push(point(m.delete_ps));
            const sub = document.getElementById('mysql-qps-subtitle');
            if (sub) sub.textContent = `QPS: ${(m.queries_ps || 0).toFixed(1)}  Sel: ${(m.select_ps || 0).toFixed(1)}  Ins: ${(m.insert_ps || 0).toFixed(1)}`;
        }

        // 3. Slow Queries
        if (!state.charts.mysqlSlow) {
            createAppChartCard('card-mysql-slow', 'chart-mysql-slow', 'mysql-slow-subtitle', 'MySQL \u2014 Slow Queries/s', APP_ORDER_MYSQL + 2);
            state.charts.mysqlSlow = createTimeSeriesChart('chart-mysql-slow', [
                { label: 'Slow Queries/s', borderColor: colors.red, backgroundColor: colors.redAlpha, fill: true, data: [] },
            ]);
        }
        if (state.charts.mysqlSlow) {
            state.charts.mysqlSlow.data.datasets[0].data.push(point(m.slow_queries_ps));
            const sub = document.getElementById('mysql-slow-subtitle');
            if (sub) sub.textContent = `Slow: ${(m.slow_queries_ps || 0).toFixed(2)}`;
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
            state.charts.mysqlInnoDB.data.datasets[0].data.push(point(m.innodb_buffer_pool_hit_pct));
            state.charts.mysqlInnoDB.data.datasets[1].data.push(point(m.innodb_bp_reads_ps));
            const sub = document.getElementById('mysql-innodb-subtitle');
            if (sub) sub.textContent = `Hit: ${(m.innodb_buffer_pool_hit_pct || 0).toFixed(1)}%  Reads/s: ${(m.innodb_bp_reads_ps || 0).toFixed(0)}`;
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
            state.charts.mysqlLocks.data.datasets[0].data.push(point(m.table_locks_waited_ps));
            state.charts.mysqlLocks.data.datasets[1].data.push(point(m.row_lock_waits_ps));
            const sub = document.getElementById('mysql-locks-subtitle');
            if (sub) sub.textContent = `Table: ${(m.table_locks_waited_ps || 0).toFixed(2)}  Row: ${(m.row_lock_waits_ps || 0).toFixed(2)}`;
        }
    }

    // Containers (3 charts per container: CPU, Memory, I/O)
    if (s.apps?.containers?.length > 0) {
        appsVisible = true;
        for (const ct of s.apps.containers) {
            const base = `container_${ct.id}`;
            const cpuKey = `${base}_cpu`;
            const memKey = `${base}_mem`;
            const ioKey  = `${base}_io`;
            const label = ct.name || ct.id;

            // CPU chart
            if (!state.containerCharts[cpuKey]) {
                createAppChartCard(`card-${cpuKey}`, `chart-${cpuKey}`, `${cpuKey}-subtitle`, `${label} \u2014 CPU`, APP_ORDER_CONTAINERS);
                state.containerCharts[cpuKey] = createTimeSeriesChart(`chart-${cpuKey}`, [
                    { label: 'CPU %', borderColor: colors.blue, backgroundColor: colors.blueAlpha, fill: true, data: [] },
                ], { beginAtZero: true, ticks: { callback: v => v + '%' } });
            }

            // Memory chart (absolute bytes)
            if (!state.containerCharts[memKey]) {
                createAppChartCard(`card-${memKey}`, `chart-${memKey}`, `${memKey}-subtitle`, `${label} \u2014 Memory`, APP_ORDER_CONTAINERS + 1);
                state.containerCharts[memKey] = createTimeSeriesChart(`chart-${memKey}`, [
                    { label: 'Used', borderColor: colors.purple, backgroundColor: colors.purpleAlpha, fill: true, data: [] },
                    { label: 'Limit', borderColor: colors.red, data: [], fill: false, borderDash: [4, 2] },
                ], { beginAtZero: true, ticks: { callback: v => formatBytesShort(v) } });
            }

            // I/O chart (Net + Block combined)
            if (!state.containerCharts[ioKey]) {
                createAppChartCard(`card-${ioKey}`, `chart-${ioKey}`, `${ioKey}-subtitle`, `${label} \u2014 I/O`, APP_ORDER_CONTAINERS + 2);
                state.containerCharts[ioKey] = createTimeSeriesChart(`chart-${ioKey}`, [
                    { label: 'Net Rx', borderColor: colors.green, data: [], fill: false },
                    { label: 'Net Tx', borderColor: colors.orange, data: [], fill: false },
                    { label: 'Disk Read', borderColor: colors.cyan, data: [], fill: false, borderDash: [4, 2] },
                    { label: 'Disk Write', borderColor: colors.pink, data: [], fill: false, borderDash: [4, 2] },
                ], { beginAtZero: true, ticks: { callback: v => formatBytesShort(v) + '/s' } });
            }

            // Push data points
            const cpuChart = state.containerCharts[cpuKey];
            if (cpuChart) {
                cpuChart.data.datasets[0].data.push(point(ct.cpu_pct || 0));
                if (!state.loadingHistory) cpuChart.update('none');
            }

            const memChart = state.containerCharts[memKey];
            if (memChart) {
                memChart.data.datasets[0].data.push(point(ct.mem_used || 0));
                memChart.data.datasets[1].data.push(point(ct.mem_limit || 0));
                if (!state.loadingHistory) memChart.update('none');
            }

            const ioChart = state.containerCharts[ioKey];
            if (ioChart) {
                ioChart.data.datasets[0].data.push(point(ct.net_rx_bps || 0));
                ioChart.data.datasets[1].data.push(point(ct.net_tx_bps || 0));
                ioChart.data.datasets[2].data.push(point(ct.disk_r_bps || 0));
                ioChart.data.datasets[3].data.push(point(ct.disk_w_bps || 0));
                if (!state.loadingHistory) ioChart.update('none');
            }

            const sub = document.getElementById(`${cpuKey}-subtitle`);
            if (sub) sub.textContent = `CPU: ${(ct.cpu_pct || 0).toFixed(1)}%  Mem: ${formatBytesShort(ct.mem_used || 0)}`;

            seenContainers.add(cpuKey);
            seenContainers.add(memKey);
            seenContainers.add(ioKey);
        }
    }
    // Hide container cards not in this sample
    Object.keys(state.containerCharts || {}).forEach(k => {
        const el = document.getElementById(`card-${k}`);
        if (seenContainers.has(k)) {
            el?.classList.remove('hidden');
        } else {
            el?.classList.add('hidden');
        }
    });

    // PostgreSQL — create charts on first data
    if (s.apps?.postgres) {
        const pg = s.apps.postgres;
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
            state.charts.pgConnStates.data.datasets[0].data.push(point(pg.active_conns));
            state.charts.pgConnStates.data.datasets[1].data.push(point(pg.idle_conns));
            state.charts.pgConnStates.data.datasets[2].data.push(point(pg.idle_in_tx_conns));
            state.charts.pgConnStates.data.datasets[3].data.push(point(pg.waiting_conns));
            state.charts.pgConnStates.data.datasets[4].data.push(point(pg.max_conns));
            const sub = document.getElementById('pg-conn-subtitle');
            if (sub) sub.textContent = `Active: ${pg.active_conns}  Idle: ${pg.idle_conns}  IdleTx: ${pg.idle_in_tx_conns}  Wait: ${pg.waiting_conns}`;
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
            state.charts.pgTPS.data.datasets[0].data.push(point(pg.tx_commit_ps));
            state.charts.pgTPS.data.datasets[1].data.push(point(pg.tx_rollback_ps));
            const sub = document.getElementById('pg-tps-subtitle');
            if (sub) sub.textContent = `Commits/s: ${(pg.tx_commit_ps || 0).toFixed(1)}  Rollbacks/s: ${(pg.tx_rollback_ps || 0).toFixed(1)}`;
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
            state.charts.pgLocks.data.datasets[0].data.push(point(pg.waiting_conns));
            state.charts.pgLocks.data.datasets[1].data.push(point(pg.deadlocks_ps));
            const sub = document.getElementById('pg-locks-subtitle');
            if (sub) sub.textContent = `Lock Waits: ${pg.waiting_conns}  Deadlocks/s: ${(pg.deadlocks_ps || 0).toFixed(2)}`;
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
            state.charts.pgTuples.data.datasets[0].data.push(point(pg.tup_fetched_ps));
            state.charts.pgTuples.data.datasets[1].data.push(point(pg.tup_returned_ps));
            state.charts.pgTuples.data.datasets[2].data.push(point(pg.tup_inserted_ps));
            state.charts.pgTuples.data.datasets[3].data.push(point(pg.tup_updated_ps));
            state.charts.pgTuples.data.datasets[4].data.push(point(pg.tup_deleted_ps));
            const sub = document.getElementById('pg-tuples-subtitle');
            if (sub) sub.textContent = `Fetched/s: ${(pg.tup_fetched_ps || 0).toFixed(1)}  Ins: ${(pg.tup_inserted_ps || 0).toFixed(1)}  Upd: ${(pg.tup_updated_ps || 0).toFixed(1)}  Del: ${(pg.tup_deleted_ps || 0).toFixed(1)}`;
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
            state.charts.pgIO.data.datasets[0].data.push(point(pg.blks_hit_ps));
            state.charts.pgIO.data.datasets[1].data.push(point(pg.blks_read_ps));
            const sub = document.getElementById('pg-io-subtitle');
            if (sub) sub.textContent = `Hit/s: ${(pg.blks_hit_ps || 0).toFixed(0)}  Read/s: ${(pg.blks_read_ps || 0).toFixed(0)}`;
        }

        // 6. Cache Hit Ratio
        if (!state.charts.pgCacheHit) {
            createAppChartCard('card-pg-cache-hit', 'chart-pg-cache-hit', 'pg-cache-subtitle', 'PostgreSQL \u2014 Cache Hit Ratio', APP_ORDER_POSTGRES + 5);
            state.charts.pgCacheHit = createTimeSeriesChart('chart-pg-cache-hit', [
                { label: 'Cache Hit %', borderColor: colors.green, backgroundColor: colors.greenAlpha, fill: true, data: [] },
            ], { min: 0, max: 100, ticks: { callback: v => v.toFixed(1) + '%' } });
        }
        if (state.charts.pgCacheHit) {
            state.charts.pgCacheHit.data.datasets[0].data.push(point(pg.blks_hit_pct));
            const sub = document.getElementById('pg-cache-subtitle');
            if (sub) sub.textContent = `Hit: ${(pg.blks_hit_pct || 0).toFixed(1)}%`;
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
            state.charts.pgTableHealth.data.datasets[0].data.push(point(pg.dead_tuples));
            state.charts.pgTableHealth.data.datasets[1].data.push(point(pg.live_tuples));
            const sub = document.getElementById('pg-table-subtitle');
            if (sub) sub.textContent = `Dead: ${(pg.dead_tuples || 0).toLocaleString()}  Live: ${(pg.live_tuples || 0).toLocaleString()}  Vacuums: ${pg.autovacuum_count || 0}`;
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
            state.charts.pgBgwriter.data.datasets[0].data.push(point(pg.buf_checkpoint_ps));
            state.charts.pgBgwriter.data.datasets[1].data.push(point(pg.buf_backend_ps));
            const sub = document.getElementById('pg-bgwriter-subtitle');
            if (sub) sub.textContent = `Checkpoint: ${(pg.buf_checkpoint_ps || 0).toFixed(1)}/s  Backend: ${(pg.buf_backend_ps || 0).toFixed(1)}/s`;
        }
    }

    // Custom metrics (dynamic charts per group, appended directly to grid)
    if (s.apps?.custom) {
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
                    tension: 0.2,
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
                        chart: new Chart(ctx, {
                            type: 'line',
                            data: { datasets },
                            options: {
                                responsive: true, maintainAspectRatio: false, animation: false,
                                interaction: { mode: 'index', intersect: false },
                                plugins: { tooltip: { position: 'awayFromCursor' } },
                                scales: {
                                    x: { type: 'time', display: true, time: { unit: 'minute' }, ticks: { maxTicksLimit: 6, maxRotation: 0, autoSkip: true } },
                                    y: yConfig,
                                },
                            }
                        }),
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
                    entry.chart.data.datasets[i].data.push(point(v));
                }
                if (!state.loadingHistory) entry.chart.update('none');

                const sub = document.getElementById(`custom-${group}-subtitle`);
                if (sub) {
                    sub.textContent = metrics.map(m => `${m.name}: ${m.value}`).join('  ');
                }
            }
            seenCustom.add(group);
        }
    }
    // Hide custom cards not in this sample
    Object.keys(state.customCharts || {}).forEach(k => {
        const el = document.getElementById(`card-custom-${k}`);
        if (seenCustom.has(k)) {
            el?.classList.remove('hidden');
        } else {
            el?.classList.add('hidden');
        }
    });

    // Show/hide applications section
    const titleEl = document.getElementById('applications-title');
    const gridEl = document.getElementById('applications-grid');
    if (appsVisible) {
        titleEl?.classList.remove('hidden');
        gridEl?.classList.remove('hidden');
    } else {
        titleEl?.classList.add('hidden');
        gridEl?.classList.add('hidden');
    }

    // Feed split charts
    addSampleToSplitCharts(s, ts);
}

// Batch-update all charts at once
export function updateAllCharts() {
    setChartTimeRange();
    if (typeof updateChartLabels === 'function') {
        updateChartLabels();
    }
    Object.values(state.charts).forEach(chart => {
        if (chart) chart.update('none');
    });
    // Also update split charts
    Object.values(state.splitCharts).forEach(typeCharts => {
        Object.values(typeCharts).forEach(chart => {
            if (chart && typeof chart.update === 'function') chart.update('none');
        });
    });
    // Also update dynamic app charts (containers, custom)
    forEachAppChart(chart => chart.update('none'));
}

// Redraw charts from the active buffer (used when selected devices change)
export function redrawChartsFromBuffer() {
    clearAllChartData();
    state.dataBuffer.forEach(item => {
        if (item._gap) {
            addGapToCharts(new Date(item.ts));
            return;
        }
        const timestampSrc = item.data || item;
        const ts = new Date(timestampSrc.ts || item.ts);
        addSampleToCharts(item.data || item, ts);
    });
    updateAllCharts();

    // Also update subtitles and gauges with the latest buffer item
    if (state.lastSample) {
        updateSubtitles(state.lastSample);
        updateSelectors(state.lastSample);
    }
}

export function trimChartsToTimeRange() {
    if (state.timeRange === null) return; // custom range — don't trim
    const cutoffMs = Date.now() - state.timeRange * 1000;

    const trimChart = (chart) => {
        if (!chart || !chart.data?.datasets) return;
        chart.data.datasets.forEach(ds => {
            if (!Array.isArray(ds.data) || ds.data.length === 0) return;
            let i = 0;
            while (i < ds.data.length && ds.data[i].x && ds.data[i].x < cutoffMs) i++;
            if (i > 0) ds.data.splice(0, i);
        });
    };

    Object.values(state.charts).forEach(trimChart);
    Object.values(state.splitCharts).forEach(typeCharts => {
        Object.values(typeCharts).forEach(trimChart);
    });
    forEachAppChart(trimChart);

    // Keep dataBuffer in sync with the displayed time window
    const cutoffDate = new Date(cutoffMs);
    let bi = 0;
    while (bi < state.dataBuffer.length && new Date(state.dataBuffer[bi].ts) < cutoffDate) bi++;
    if (bi > 0) state.dataBuffer.splice(0, bi);
}

export function clearAllChartData() {
    const clearChart = (chart) => {
        if (!chart?.data?.datasets) return;
        chart.data.datasets.forEach(ds => {
            if (Array.isArray(ds.data)) ds.data = [];
        });
    };
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

// tryZoomFromBuffer attempts to satisfy a zoom request from the in-memory
// data buffer, avoiding a network round-trip when the buffer already covers
// the requested window. Returns true if the redraw succeeded.
export function tryZoomFromBuffer(fromDate, toDate) {
    if (!state.dataBuffer || state.dataBuffer.length === 0) return false;

    const fromMs = fromDate.getTime();
    const toMs   = toDate.getTime();

    // Determine the time span of the current buffer.
    const first = state.dataBuffer[0];
    const last  = state.dataBuffer[state.dataBuffer.length - 1];
    const bufStart = new Date(first.ts || first.data?.ts).getTime();
    const bufEnd   = new Date(last.ts  || last.data?.ts).getTime();

    if (isNaN(bufStart) || isNaN(bufEnd)) return false;
    if (bufStart > fromMs || bufEnd < toMs) return false; // buffer doesn't cover window

    // Buffer covers the window — redraw directly from it.
    state.timeRange = null;
    state.customFrom = fromDate;
    state.customTo = toDate;

    clearAllChartData();
    const visible = state.dataBuffer.filter(item => {
        const t = new Date(item.ts || item.data?.ts).getTime();
        return !isNaN(t) && t >= fromMs && t <= toMs;
    });
    visible.forEach(item => {
        const ts = new Date(item.ts || item.data?.ts);
        addSampleToCharts(item, ts);
    });
    updateAllCharts();
    return true;
}

export function syncZoom(sourceChart) {
    const { min, max } = sourceChart.scales.x;

    // Update the display to show the zoomed timeframe explicitly
    if (min && max) {
        state.timeRange = null; // Exit preset range immediately on interaction
        const fmt = d => d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' });
        document.getElementById('time-range-display').textContent = `${fmt(new Date(min))} \u2192 ${fmt(new Date(max))} (Zoomed)`;
    }

    const windowSec = (max - min) / 1000;
    const minUnit = windowSec >= 259200 ? 'day' : false; // 3 days

    Object.values(state.charts).forEach(chart => {
        if (!chart?.options?.scales?.x) return;
        
        if (minUnit) {
            chart.options.scales.x.time.minUnit = minUnit;
        } else {
            delete chart.options.scales.x.time.minUnit;
        }

        if (chart !== sourceChart) {
            chart.options.scales.x.min = min;
            chart.options.scales.x.max = max;
            chart.update('none');
        } else {
            chart.update('none');
        }
    });

    // Sync split charts too
    Object.values(state.splitCharts).forEach(typeCharts => {
        Object.values(typeCharts).forEach(chart => {
            if (!chart?.options?.scales?.x) return;
            if (minUnit) {
                chart.options.scales.x.time.minUnit = minUnit;
            } else {
                delete chart.options.scales.x.time.minUnit;
            }
            chart.options.scales.x.min = min;
            chart.options.scales.x.max = max;
            chart.update('none');
        });
    });
    // Sync dynamic app charts
    forEachAppChart(chart => {
        if (!chart?.options?.scales?.x) return;
        if (minUnit) {
            chart.options.scales.x.time.minUnit = minUnit;
        } else {
            delete chart.options.scales.x.time.minUnit;
        }
        chart.options.scales.x.min = min;
        chart.options.scales.x.max = max;
        chart.update('none');
    });

    // When zooming or panning, fetch the optimal data resolution for the new view.
    // If we're already at max resolution (raw tier) and the buffer completely covers
    // the window, we can skip the network request.
    if (min && max) {
        const fromDate = new Date(min);
        const toDate   = new Date(max);

        if (state.currentTier === 0 && tryZoomFromBuffer(fromDate, toDate)) {
            return;
        }

        clearTimeout(_zoomFetchTimer);
        _zoomFetchTimer = setTimeout(() => {
            fetchZoomedHistory(fromDate, toDate);
        }, 150);
    }
}

// Fetch higher-resolution data for a zoomed window and replace chart data,
// then re-apply the zoom so the viewport stays exactly where the user dragged.
export function fetchZoomedHistory(fromDate, toDate) {
    if (state.loadingHistory) return;
    state.loadingHistory = true;
    document.getElementById('loading-spinner')?.classList.remove('hidden');

    const from = fromDate.toISOString();
    const to = toDate.toISOString();
    const points = Math.max(600, window.innerWidth || 1000);
    fetch(apiUrl(`/api/history?from=${from}&to=${to}&points=${points}`))
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
                state.lastHistoricalTs = new Date(s.ts || lastSample.ts);
                updateGauges(s);
                updateHeader(s);
                updateSubtitles(s);
                evaluateAlerts(s);
            }

            // Re-apply the zoom viewport so the user stays in the same window
            const minMs = fromDate.getTime();
            const maxMs = toDate.getTime();
            const reapplyZoom = (chart) => {
                if (!chart?.options?.scales?.x) return;
                chart.options.scales.x.min = minMs;
                chart.options.scales.x.max = maxMs;
                chart.update('none');
            };
            Object.values(state.charts).forEach(reapplyZoom);
            forEachAppChart(reapplyZoom);

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
                document.dispatchEvent(new Event('kula-sync-pause'));
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

export function resetZoomAll() {
    const resetChart = (chart) => {
        if (!chart?.options?.scales?.x) return;
        delete chart.options.scales.x.min;
        delete chart.options.scales.x.max;
        delete chart.options.scales.x.time.minUnit;
        chart.update('none');
    };
    Object.values(state.charts).forEach(resetChart);
    Object.values(state.splitCharts).forEach(typeCharts => {
        Object.values(typeCharts).forEach(resetChart);
    });
    forEachAppChart(resetChart);

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
        document.dispatchEvent(new Event('kula-sync-pause'));
    }
}

// ---- Gap Insertion ----
export function insertGapsInHistory(data, resolutionStr = '1s') {
    if (state.joinMetrics || data.length < 2) return data;

    let expectedInterval = 1000; // default 1s
    if (typeof resolutionStr === 'string') {
        const num = parseInt(resolutionStr) || 1;
        if (resolutionStr.endsWith('s')) expectedInterval = num * 1000;
        else if (resolutionStr.endsWith('m')) expectedInterval = num * 60000;
        else if (resolutionStr.endsWith('h')) expectedInterval = num * 3600000;
    }

    // Auto-detect actual data interval from the median gap between the first
    // samples.  The storage resolution can be "1s" even when collection.interval
    // is e.g. 5s, which would cause every normal gap to be misclassified.
    const sampleLimit = Math.min(data.length - 1, 20);
    if (sampleLimit >= 3) {
        const gaps = [];
        for (let i = 0; i < sampleLimit; i++) {
            const a = new Date(data[i].ts || data[i].data?.ts).getTime();
            const b = new Date(data[i + 1].ts || data[i + 1].data?.ts).getTime();
            gaps.push(b - a);
        }
        gaps.sort((a, b) => a - b);
        const median = gaps[Math.floor(gaps.length / 2)];
        if (median > expectedInterval) {
            expectedInterval = median;
        }
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

export function addGapToCharts(ts) {
    const addGap = (chart) => {
        if (!chart?.data?.datasets) return;
        chart.data.datasets.forEach(ds => {
            if (Array.isArray(ds.data)) ds.data.push({ x: ts, y: null });
        });
    };
    Object.values(state.charts).forEach(addGap);
    Object.values(state.splitCharts).forEach(typeCharts => {
        Object.values(typeCharts).forEach(addGap);
    });
    forEachAppChart(addGap);
}

// ---- Device Selectors ----
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
                    redrawChartsFromBuffer();
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
                    redrawChartsFromBuffer();
                };
            }
        }
    }

    if (s.disk && s.disk.devices) {
        const devs = s.disk.devices.map(d => d.name).sort();
        if (devs.join(',') !== state.diskIoOptions.join(',')) {
            state.diskIoOptions = devs;
            const sel = el('diskio-selector');
            if (sel) {
                if (!state.selectedDiskIo || !devs.includes(state.selectedDiskIo)) {
                    state.selectedDiskIo = devs[0] || '';
                    localStorage.setItem('kula_sel_diskio', state.selectedDiskIo);
                }
                sel.innerHTML = '';
                devs.forEach(d => {
                    const opt = document.createElement('option');
                    opt.value = d;
                    opt.textContent = d;
                    sel.appendChild(opt);
                });
                sel.value = state.selectedDiskIo;
                sel.classList.toggle('no-arrow', devs.length <= 1);
                sel.classList.remove('hidden');
                sel.onchange = (e) => {
                    state.selectedDiskIo = e.target.value;
                    localStorage.setItem('kula_sel_diskio', state.selectedDiskIo);
                    redrawChartsFromBuffer();
                };
            }
        }
    }

    if (s.disk && s.disk.devices) {
        const tempDevs = s.disk.devices.filter(d => d.temp > 0 || (d.sensors && d.sensors.length > 0)).map(d => d.name).sort();
        if (tempDevs.join(',') !== state.diskTempOptions.join(',')) {
            state.diskTempOptions = tempDevs;
            const sel = el('disktemp-selector');
            if (sel) {
                if (!state.selectedDiskTemp || !tempDevs.includes(state.selectedDiskTemp)) {
                    state.selectedDiskTemp = tempDevs[0] || '';
                    if (state.selectedDiskTemp) {
                        localStorage.setItem('kula_sel_disktemp', state.selectedDiskTemp);
                    }
                }
                sel.innerHTML = '';
                tempDevs.forEach(d => {
                    const opt = document.createElement('option');
                    opt.value = d;
                    opt.textContent = d;
                    sel.appendChild(opt);
                });
                sel.value = state.selectedDiskTemp;
                sel.classList.toggle('no-arrow', tempDevs.length <= 1);
                sel.classList.remove('hidden');
                sel.onchange = (e) => {
                    state.selectedDiskTemp = e.target.value;
                    localStorage.setItem('kula_sel_disktemp', state.selectedDiskTemp);
                    redrawChartsFromBuffer();
                };
            }
        }
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
                    redrawChartsFromBuffer();
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
                        redrawChartsFromBuffer();
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
    const ts = new Date(sample.ts || sample.data?.ts);

    // Prevent duplicate or out-of-order samples
    if (state.lastSample) {
        const lastTs = new Date(state.lastSample.ts || state.lastSample.data?.ts);
        if (ts.getTime() <= lastTs.getTime()) {
            return;
        }
    }

    state.dataBuffer.push(sample);
    state.lastSample = sample;
    if (state.dataBuffer.length > state.maxBufferSize) {
        state.dataBuffer.shift();
    }

    updateSelectors(sample);
    updateGauges(sample);
    updateHeader(sample);
    addSampleToCharts(sample, ts); // For live sample, item is the sample itself
    trimChartsToTimeRange();
    updateAllCharts();
    updateSubtitles(sample);
    evaluateAlerts(sample);
}

export function updateSamplingInfo(tier, resolution) {
    const el = document.getElementById('sampling-info');
    if (!el) return;
    const tierNames = ['Tier 1 (raw)', 'Tier 2 (aggregated)', 'Tier 3 (long-term)'];
    const name = tierNames[tier] || `Tier ${tier + 1}`;
    el.textContent = `${resolution} samples · ${name}`;
    state.currentResolution = resolution || '1s';
    state.currentTier = tier;

    const aggList = document.getElementById('agg-presets-list');
    const aggDiv = document.getElementById('agg-divider');
    const aggBtnMobile = document.getElementById('btn-agg-menu');

    if (aggList && aggDiv) {
        if (tier === 0) {
            aggList.classList.add('hidden');
            aggDiv.classList.add('hidden');
            if (aggBtnMobile) aggBtnMobile.classList.add('hidden');
        } else {
            aggList.classList.remove('hidden');
            aggDiv.classList.remove('hidden');
            if (aggBtnMobile) aggBtnMobile.classList.remove('hidden');
        }
    }
}

export function fetchHistory(rangeSeconds) {
    if (state.loadingHistory) return;
    state.loadingHistory = true;
    document.getElementById('loading-spinner')?.classList.remove('hidden');

    const to = new Date().toISOString();
    const from = new Date(Date.now() - rangeSeconds * 1000).toISOString();
    const points = Math.max(600, window.innerWidth || 1000);
    fetch(apiUrl(`/api/history?from=${from}&to=${to}&points=${points}`))
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

            // Pre-calculate selectors from newest sample so charting has correct selection
            const lastItemH = data[data.length - 1];
            updateSelectors(lastItemH.data || lastItemH);

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
                const timestampSrc = item.data || item;
                const ts = new Date(timestampSrc.ts || item.ts);
                state.dataBuffer.push(item);
                addSampleToCharts(item, ts);
            });

            // Trim buffer
            if (state.dataBuffer.length > state.maxBufferSize) {
                state.dataBuffer = state.dataBuffer.slice(-state.maxBufferSize);
            }

            // Single batch update of all charts
            trimChartsToTimeRange();
            updateAllCharts();

            // Update gauges/header with latest sample
            const lastItem = data[data.length - 1];
            const s = lastItem.data || lastItem;
            state.lastSample = s;
            state.lastHistoricalTs = new Date(s.ts || lastItem.ts);
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

export function fetchCustomHistory(fromDate, toDate) {
    if (state.loadingHistory) return;
    state.loadingHistory = true;
    document.getElementById('loading-spinner')?.classList.remove('hidden');

    const from = fromDate.toISOString();
    const to = toDate.toISOString();
    const points = Math.max(600, window.innerWidth || 1000);
    fetch(apiUrl(`/api/history?from=${from}&to=${to}&points=${points}`))
        .then(r => r.json())
        .then(response => {
            const data = response.samples || response;
            const isEnvelope = response.samples !== undefined;

            if (isEnvelope) {
                updateSamplingInfo(response.tier, response.resolution);
            }

            if (Array.isArray(data) && data.length > 0) {
                const lastItemC = data[data.length - 1];
                updateSelectors(lastItemC.data || lastItemC);
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
                    const timestampSrc = item.data || item;
                    const ts = new Date(timestampSrc.ts || item.ts);
                    state.dataBuffer.push(item);
                    addSampleToCharts(item, ts);
                });

                if (state.dataBuffer.length > state.maxBufferSize) {
                    state.dataBuffer = state.dataBuffer.slice(-state.maxBufferSize);
                }

                const lastItem = data[data.length - 1];
                const s = lastItem.data || lastItem;
                state.lastSample = s;
                state.lastHistoricalTs = new Date(s.ts || lastItem.ts);
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

export function fetchGapHistory(fromDate, toDate) {
    if (state.loadingHistory) return;
    state.loadingHistory = true;

    const from = fromDate.toISOString();
    const to = toDate.toISOString();
    fetch(apiUrl(`/api/history?from=${from}&to=${to}&points=300`))
        .then(r => r.json())
        .then(response => {
            const data = response.samples || response;
            if (Array.isArray(data) && data.length > 0) {
                const lastItemC = data[data.length - 1];
                updateSelectors(lastItemC.data || lastItemC);

                const processed = insertGapsInHistory(data, response?.resolution);
                const existingTs = new Set(state.dataBuffer.map(i => new Date(i.ts || i.data?.ts).getTime()));
                let added = 0;

                processed.forEach(item => {
                    const tsMs = new Date(item.ts || item.data?.ts).getTime();
                    if (tsMs <= fromDate.getTime() || existingTs.has(tsMs)) return;

                    state.dataBuffer.push(item);
                    added++;
                });

                if (added > 0) {
                    state.dataBuffer.sort((a,b) => new Date(a.ts || a.data?.ts).getTime() - new Date(b.ts || b.data?.ts).getTime());
                    if (state.dataBuffer.length > state.maxBufferSize) {
                        state.dataBuffer = state.dataBuffer.slice(-state.maxBufferSize);
                    }
                    redrawChartsFromBuffer();
                }

                if (state.dataBuffer.length > state.maxBufferSize) {
                    state.dataBuffer = state.dataBuffer.slice(-state.maxBufferSize);
                }

                const lastItem = data[data.length - 1];
                const s = lastItem.data || lastItem;
                state.lastSample = s;
                state.lastHistoricalTs = new Date(s.ts || lastItem.ts);

                trimChartsToTimeRange();
                updateAllCharts();
                updateGauges(s);
                updateHeader(s);
                updateSubtitles(s);
                evaluateAlerts(s);
            }
            state.loadingHistory = false;
            drainLiveQueue();
        })
        .catch(e => {
            console.error('Gap history fetch error:', e);
            state.loadingHistory = false;
            drainLiveQueue();
        });
}



// Replay any samples that arrived while history was loading.
export function drainLiveQueue() {
    if (state.liveQueue.length === 0) return;
    const queue = state.liveQueue;
    state.liveQueue = [];
    queue.forEach(sample => {
        // Skip samples whose timestamp was already covered by the history load
        if (state.lastHistoricalTs && new Date(sample.ts) <= state.lastHistoricalTs) return;
        pushLiveSample(sample);
    });
}
