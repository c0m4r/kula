/* ============================================================
   state.js — Shared application state, color palette, and
   Chart.js global configuration.
   Must be loaded FIRST before all other modules.
   ============================================================ */
'use strict';

// ---- State ----
export const state = {
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
    aggDropdownOpen: false,
    layoutMode: localStorage.getItem('kula_layout') || 'grid',
    lastSample: null,
    joinMetrics: false, // fetched from server config
    focusMode: false,
    focusSelecting: false,
    focusVisible: JSON.parse(localStorage.getItem('kula_focus_visible') || 'null'),
    currentResolution: '1s', // resolution of data currently loaded in charts
    liveQueue: [],        // samples buffered while history is loading
    theme: localStorage.getItem('kula_theme') || 'dark',
    diskSpaceMountNames: [], // Not used as datasets anymore, but kept for compatibility
    cpuTempSensorNames: [],
    diskTempSensorNames: [],
    currentAggregation: localStorage.getItem('kula_aggregation') || 'avg',
    netOptions: [],
    diskIoOptions: [],
    diskTempOptions: [],
    diskSpaceOptions: [],
    gpuLoadOptions: [],
    selectedNet: localStorage.getItem('kula_sel_net') || null,
    selectedDiskIo: localStorage.getItem('kula_sel_diskio') || null,
    selectedDiskTemp: localStorage.getItem('kula_sel_disktemp') || null,
    selectedDiskSpace: localStorage.getItem('kula_sel_diskspace') || null,
    selectedGpuLoad: localStorage.getItem('kula_sel_gpuload') || null,
    selectedVram: localStorage.getItem('kula_sel_vram') || null,
    selectedGpuTemp: localStorage.getItem('kula_sel_gputemp') || null,
    configMax: {}, // loaded from server /api/config
    lastHistoricalTs: null,
    splitNet: JSON.parse(localStorage.getItem('kula_split_net') || 'false'),
    splitDiskIo: JSON.parse(localStorage.getItem('kula_split_diskio') || 'false'),
    splitDiskSpace: JSON.parse(localStorage.getItem('kula_split_diskspace') || 'false'),
    splitDiskTemp: JSON.parse(localStorage.getItem('kula_split_disktemp') || 'false'),
    splitGpu: JSON.parse(localStorage.getItem('kula_split_gpu') || 'false'),
    splitCharts: {}, // { type: { chartKey: chartInstance } }
};

// ---- Color Palette ----
export const colors = {
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

// ---- Custom Tooltip Position: keep tooltip away from cursor ----
Chart.registry.plugins.get('tooltip'); // ensure plugin is ready
Chart.Tooltip.positioners.awayFromCursor = function (elements, eventPosition) {
    const chart = this.chart;
    const tooltipWidth = this.width || 180;
    const tooltipHeight = this.height || 80;
    const offset = 18; // gap between tooltip and cursor

    // Mouse position relative to canvas
    const mx = eventPosition.x;
    const my = eventPosition.y;

    // Try right of cursor first, fallback to left
    let x = mx + offset;
    if (x + tooltipWidth > chart.chartArea.right + 10) {
        x = mx - tooltipWidth - offset;
    }
    // Clamp horizontally within canvas
    x = Math.max(0, Math.min(x, chart.width - tooltipWidth));

    // Vertically: prefer above cursor, fallback to below
    let y = my - tooltipHeight - offset;
    if (y < 0) {
        y = my + offset;
    }
    // Clamp vertically within canvas
    y = Math.max(0, Math.min(y, chart.height - tooltipHeight));

    return { x, y };
};

// ---- Shared Helpers ----
export const escapeHTML = (str) => String(str).replace(/[&<>"']/g, m => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[m]));

export function getChartMaxBound(id) {
    let pref = {};
    try { pref = JSON.parse(localStorage.getItem('kula_graphs_max') || '{}')[id]; } catch (e) { }
    if (!pref && state.configMax) pref = state.configMax[id];
    if (!pref || !pref.mode || pref.mode === 'off') return undefined;
    if (pref.mode === 'on') return pref.value;
    if (pref.mode === 'auto') {
        if (typeof pref.auto === 'number' && pref.auto > 0) return pref.auto; // TjMax or Link Speed
        return pref.value; // Fallback bound
    }
    return undefined;
}
