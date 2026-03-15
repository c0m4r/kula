/* ============================================================
   charts-data.js — Chart data ingestion, updates, zoom sync,
   gap insertion, device selectors, and the live sample pipeline.
   ============================================================ */
'use strict';

// ---- Data Update ----
function addSampleToCharts(item, ts) {
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

    // Network (selected non-lo interface)
    if (state.charts.network && s.net?.ifaces) {
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

    // Packets per second (selected non-lo interface)
    if (state.charts.pps && s.net?.ifaces) {
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
    }

    // Disk I/O (selected device)
    if (state.charts.diskio && s.disk?.devices) {
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

    // Disk Temperature
    const diskTempCard = document.getElementById('card-disk-temp');
    if (state.charts.disktemp && s.disk?.devices) {
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

    // Disk Space — single dataset for selected mount
    if (state.charts.diskspace && s.disk?.filesystems && s.disk.filesystems.length > 0) {
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

    // GPU Metrics
    if (s.gpu && s.gpu.length > 0) {
        const g = s.gpu.find(g => g.name === state.selectedGpuLoad) || s.gpu[0];
        if (state.charts.gpuload) {
            document.getElementById('card-gpu-load')?.classList.remove('hidden');
            state.charts.gpuload.data.datasets[0].data.push(point(g.load_pct || 0));
            state.charts.gpuload.data.datasets[1].data.push(point(g.power_w || 0));
        }
        if (state.charts.vram) {
            document.getElementById('card-vram')?.classList.remove('hidden');
            state.charts.vram.data.datasets[0].data.push(point(g.vram_used || 0));
            state.charts.vram.options.scales.y.max = g.vram_total > 0 ? g.vram_total : undefined;
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
}

// Batch-update all charts at once
function updateAllCharts() {
    setChartTimeRange();
    Object.values(state.charts).forEach(chart => {
        if (chart) chart.update('none');
    });
}

// Redraw charts from the active buffer (used when selected devices change)
function redrawChartsFromBuffer() {
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
    const points = Math.max(600, window.innerWidth || 1000);
    fetch(`/api/history?from=${from}&to=${to}&points=${points}`)
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

// ---- Device Selectors ----
function updateSelectors(s) {
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
    }
}

// ---- Live Sample Pipeline ----
// Push a single live sample — adds data + updates charts immediately
function pushLiveSample(sample) {
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
