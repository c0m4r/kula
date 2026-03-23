/* ============================================================
   controls.js — Pause/resume, layout toggle, time range
   selection, and history fetching.
   ============================================================ */
'use strict';
import { state } from './state.js';
import { i18n } from './i18n.js';
import { initCharts } from './charts-init.js';
import { resetZoomAll, fetchHistory, fetchCustomHistory } from './charts-data.js';

// ---- Pause/Resume ----
export function syncPauseState() {
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

export function togglePause() {
    state.pausedManual = !state.pausedManual;
    syncPauseState();
}

// ---- Layout Toggle ----
export function toggleLayout() {
    state.layoutMode = state.layoutMode === 'grid' ? 'list' : 'grid';
    localStorage.setItem('kula_layout', state.layoutMode);
    applyLayout();
}

export function applyLayout() {
    const dashboard = document.getElementById('dashboard');
    const btn = document.getElementById('btn-layout');

    if (state.layoutMode === 'list') {
        dashboard.classList.add('layout-list');
        btn.classList.add('layout-active');
        btn.textContent = '⊟';
        btn.title = i18n.t('switch_grid');
    } else {
        dashboard.classList.remove('layout-list');
        btn.classList.remove('layout-active');
        btn.textContent = '⊞';
        btn.title = i18n.t('switch_list');
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
export function setTimeRange(seconds) {
    state.timeRange = seconds;
    state.customFrom = null;
    state.customTo = null;
    document.querySelectorAll('.time-btn[data-range]').forEach(b => b.classList.remove('active'));
    document.querySelector(`.time-btn[data-range="${seconds}"]`)?.classList.add('active');
    document.getElementById('btn-custom-range')?.classList.remove('active');

    const labels = {
        60: i18n.t('last_1_m'), 300: i18n.t('last_5_m'), 900: i18n.t('last_15_m'), 1800: i18n.t('last_30_m'),
        3600: i18n.t('last_1_h'), 10800: i18n.t('last_3_h'), 21600: i18n.t('last_6_h'), 43200: i18n.t('last_12_h'),
        86400: i18n.t('last_24_h'), 259200: i18n.t('last_3_d'), 604800: i18n.t('last_7_d'), 2592000: i18n.t('last_30_d')
    };
    document.getElementById('time-range-display').textContent = labels[seconds] || `${i18n.t('last')} ${seconds}s`;

    resetZoomAll();
    fetchHistory(seconds);
}

// ---- Custom Time Range ----
export function toggleCustomTimePicker() {
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

export function applyCustomRange() {
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

export function toLocalISOString(date) {
    const pad = n => String(n).padStart(2, '0');
    return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}
