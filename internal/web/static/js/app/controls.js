/* ============================================================
   controls.js — Pause/resume, layout toggle, time range
   selection, and history fetching.
   ============================================================ */
'use strict';

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

    const aggList = document.getElementById('agg-presets-list');
    const aggDiv = document.getElementById('agg-divider');
    const aggBtnMobile = document.getElementById('btn-agg-menu');

    if (aggList && aggDiv) {
        if (tier === 0 && resolution === '1s') {
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

function fetchHistory(rangeSeconds) {
    if (state.loadingHistory) return;
    state.loadingHistory = true;
    document.getElementById('loading-spinner')?.classList.remove('hidden');

    const to = new Date().toISOString();
    const from = new Date(Date.now() - rangeSeconds * 1000).toISOString();
    const points = Math.max(600, window.innerWidth || 1000);
    fetch(`/api/history?from=${from}&to=${to}&points=${points}`)
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

function fetchCustomHistory(fromDate, toDate) {
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
