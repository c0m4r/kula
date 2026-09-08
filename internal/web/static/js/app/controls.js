/* ============================================================
   controls.js — Pause/resume, layout toggle, time range
   selection, and history fetching.
   ============================================================ */
'use strict';
import { state } from './state.js';
import { apiUrl } from './api.js';
import { i18n } from './i18n.js';
import { forEachRegisteredChart, queueAllChartUpdates } from './chart-controller.js';
import {
    fetchCustomHistory,
    fetchHistory,
    resetZoomAll,
} from './charts-data.js';
import { updateUrl, ViewportHistory, zoomOutInterval } from './history-navigation.js';
import { attachRangeCalendar } from './date-range-calendar.js';
import {
    formatDateTimeInput,
    formatRangeTimestamp,
    parseDateTimeInput,
} from './format.js';

const viewportHistory = new ViewportHistory();

function currentViewport() {
    if (state.timeRange !== null) return { kind: 'preset', range: state.timeRange };
    if (state.customFrom && state.customTo) {
        return { kind: 'custom', from: state.customFrom, to: state.customTo };
    }
    return null;
}

function updateNavigationControls() {
    const back = document.getElementById('btn-history-back');
    const forward = document.getElementById('btn-history-forward');
    const zoomOut = document.getElementById('btn-zoom-out');
    if (back) back.disabled = !viewportHistory.canBack();
    if (forward) forward.disabled = !viewportHistory.canForward();
    if (zoomOut) zoomOut.disabled = !(state.customFrom && state.customTo);
    document.getElementById('btn-live')?.classList.toggle('active', state.timeRange !== null);
}

function recordViewport() {
    const viewport = currentViewport();
    if (viewport) viewportHistory.push(viewport);
    updateNavigationControls();
}

export function initializeViewportNavigation() {
    if (state.timeRange !== null) state.lastPresetRange = state.timeRange;
    recordViewport();
}



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

    // Keep chart instances, data and legend selections across CSS layout changes.
    // Chart.js observes canvas parents; an explicit resize also handles hidden
    // cards when they next become visible.
    requestAnimationFrame(() => {
        forEachRegisteredChart(chart => chart.resize());
        queueAllChartUpdates();
    });
}

export function reloadCurrentHistory() {
    if (state.timeRange !== null) {
        return fetchHistory(state.timeRange);
    }
    if (state.customFrom && state.customTo) {
        return fetchCustomHistory(state.customFrom, state.customTo);
    }
    return null;
}

// ---- Time Range ----
export function setTimeRange(seconds, { record = true } = {}) {
    state.timeRange = seconds;
    state.historyViewEnd = null;
    state.lastPresetRange = seconds;
    state.customFrom = null;
    state.customTo = null;
    closeCustomTimePicker(false);
    syncTimeRangeUI(seconds);
    updateUrl(state);
    if (record) recordViewport();
    else updateNavigationControls();

    resetZoomAll();
    fetchHistory(seconds);
}

// syncTimeRangeUI updates the active preset button and the range display
// label to reflect the given preset window, without fetching history.
// Shared by setTimeRange and the URL-state restore on page load.
export function syncTimeRangeUI(seconds) {
    document.querySelectorAll('.time-btn[data-range]').forEach(b => b.classList.remove('active'));
    document.querySelector(`.time-btn[data-range="${seconds}"]`)?.classList.add('active');
    document.getElementById('btn-custom-range')?.classList.remove('active');

    const labels = {
        60: i18n.t('last_1_m'), 300: i18n.t('last_5_m'), 900: i18n.t('last_15_m'), 1800: i18n.t('last_30_m'),
        3600: i18n.t('last_1_h'), 10800: i18n.t('last_3_h'), 21600: i18n.t('last_6_h'), 43200: i18n.t('last_12_h'),
        86400: i18n.t('last_24_h'), 259200: i18n.t('last_3_d'), 604800: i18n.t('last_7_d'), 2592000: i18n.t('last_30_d')
    };
    const display = document.getElementById('time-range-display');
    display?.removeAttribute('data-i18n');
    if (display) display.textContent = labels[seconds] || `${i18n.t('last')} ${seconds}s`;
}

// ---- Custom Time Range ----
const MAX_CUSTOM_RANGE_MS = 31 * 86400000;
let rangeCalendar;
let pickerBoundsLoading = false;
let pickerBoundsError = false;
let pickerBoundsRequest = 0;

function pickerDate(date) {
    return formatDateTimeInput(date, state.timeZone, true, state.collectionIntervalMs < 1000);
}

function retainedRanges() {
    const quantum = state.collectionIntervalMs < 1000 ? 1 : 1000;
    return (state.retainedRanges || []).map(range => ({
        from: Math.ceil(Date.parse(range.from) / quantum) * quantum,
        to: Math.floor(Math.min(Date.parse(range.to), Date.now()) / quantum) * quantum,
    })).filter(range => Number.isFinite(range.from) && Number.isFinite(range.to) && range.from <= range.to);
}

function retainedDay(day) {
    return retainedRanges().some(range => day >= pickerDate(range.from).slice(0, 10) &&
        day <= pickerDate(range.to).slice(0, 10));
}

function clampToRetention(date) {
    const ranges = retainedRanges();
    if (ranges.length === 0) return date;
    const timestamp = date.getTime();
    const nearest = ranges.map(range => Math.max(range.from, Math.min(range.to, timestamp)))
        .sort((a, b) => Math.abs(a - timestamp) - Math.abs(b - timestamp))[0];
    return new Date(nearest);
}

function syncPickerLimits() {
    const ranges = retainedRanges();
    for (const id of ['custom-from', 'custom-to']) {
        const input = document.getElementById(id);
        input.step = state.collectionIntervalMs < 1000 ? '0.001' : '1';
        input.min = ranges.length ? pickerDate(Math.min(...ranges.map(range => range.from))) : '';
        input.max = ranges.length ? pickerDate(Math.max(...ranges.map(range => range.to))) : '';
    }
}

async function refreshPickerBounds() {
    const request = ++pickerBoundsRequest;
    const initialFrom = document.getElementById('custom-from').value;
    const initialTo = document.getElementById('custom-to').value;
    pickerBoundsLoading = true;
    pickerBoundsError = false;
    updateCustomRangePreview();
    rangeCalendar?.sync();
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 10000);
    try {
        const response = await fetch(apiUrl('/api/config'), { cache: 'no-store', signal: controller.signal });
        if (!response.ok) throw new Error('History bounds unavailable');
        const config = await response.json();
        if (!Array.isArray(config.history?.ranges)) throw new Error('History bounds unavailable');
        if (request !== pickerBoundsRequest) return;
        state.retainedRanges = config.history.ranges;
        state.collectionIntervalMs = config.history.collection_interval_ms || 1000;
        syncPickerLimits();
        if (!document.getElementById('time-custom').classList.contains('hidden')) {
            const draft = customRangeDraft();
            if (draft.from && draft.to && document.getElementById('custom-from').value === initialFrom &&
                document.getElementById('custom-to').value === initialTo) {
                setPickerDates(clampToRetention(draft.from), clampToRetention(draft.to));
            }
            rangeCalendar?.sync(true);
        }
    } catch (_error) {
        if (request === pickerBoundsRequest) pickerBoundsError = true;
    } finally {
        clearTimeout(timeout);
        if (request === pickerBoundsRequest) {
            pickerBoundsLoading = false;
            updateCustomRangePreview();
            rangeCalendar?.sync();
        }
    }
}

function closeCustomTimePicker(restoreFocus = true) {
    const picker = document.getElementById('time-custom');
    const wasOpen = picker && !picker.classList.contains('hidden');
    picker?.classList.add('hidden');
    const button = document.getElementById('btn-custom-range');
    button?.setAttribute('aria-expanded', 'false');
    button?.classList.toggle('active', state.timeRange === null);
    if (wasOpen && restoreFocus) button?.focus();
}

function setPickerDates(from, to, syncCalendar = true) {
    syncPickerLimits();
    document.getElementById('custom-from').value = pickerDate(from);
    document.getElementById('custom-to').value = pickerDate(to);
    updateCustomRangePreview();
    if (syncCalendar) rangeCalendar?.sync(true);
}

function customRangeDraft() {
    const fromVal = document.getElementById('custom-from').value;
    const toVal = document.getElementById('custom-to').value;
    const from = parseDateTimeInput(fromVal, state.timeZone);
    const to = parseDateTimeInput(toVal, state.timeZone);
    const available = date => retainedRanges().some(range => date >= range.from && date <= range.to);
    const error = !from || !to ? 'range_dates_required'
        : from >= to ? 'range_start_before_end'
        : to - from > MAX_CUSTOM_RANGE_MS ? 'range_max_31_days'
        : state.collectionIntervalMs >= 1000 && (from.getMilliseconds() || to.getMilliseconds()) ? 'range_whole_seconds'
        : pickerBoundsLoading ? 'history_loading'
        : pickerBoundsError ? 'history_failed'
        : retainedRanges().length === 0 ? 'history_empty'
        : !available(from) || !available(to) ? 'range_outside_retention' : null;
    return { from, to, error };
}

function updateCustomRangePreview() {
    const draft = customRangeDraft();
    document.querySelector('#time-custom button[type="submit"]').disabled = !!draft.error;
    const error = document.getElementById('custom-range-error');
    error.textContent = draft.error ? i18n.t(draft.error) : '';
    error.classList.toggle('hidden', !draft.error);
    const inputError = ['range_dates_required', 'range_start_before_end', 'range_max_31_days',
        'range_whole_seconds', 'range_outside_retention'].includes(draft.error);
    document.getElementById('custom-from').setAttribute('aria-invalid', String(!draft.from || inputError));
    document.getElementById('custom-to').setAttribute('aria-invalid', String(!draft.to || inputError));
    const summary = document.getElementById('custom-range-summary');
    if (draft.error) {
        summary.textContent = '';
        return;
    }
    let minutes = Math.round((draft.to - draft.from) / 60000);
    const parts = [];
    for (const [unit, size] of [['day', 1440], ['hour', 60], ['minute', 1]]) {
        const value = Math.floor(minutes / size);
        minutes %= size;
        if (value) parts.push(new Intl.NumberFormat(i18n.currentLang, {
            style: 'unit', unit, unitDisplay: 'short',
        }).format(value));
    }
    summary.textContent = `${i18n.t('selected_duration')}: ${parts.join(' ')}`;
}

// Convert an open draft when the dashboard's display zone changes. Unsaved
// dates continue to refer to the same instants, rather than shifting in time.
export function refreshCustomTimePicker(previousZone = state.timeZone) {
    const zone = document.getElementById('custom-time-zone');
    if (zone) zone.textContent = state.timeZone === 'utc' ? 'UTC'
        : `${i18n.t('time_zone_local')} · ${Intl.DateTimeFormat().resolvedOptions().timeZone}`;
    if (document.getElementById('time-custom')?.classList.contains('hidden')) return;
    if (previousZone !== state.timeZone) {
        for (const id of ['custom-from', 'custom-to']) {
            const input = document.getElementById(id);
            const date = parseDateTimeInput(input.value, previousZone);
            if (date) input.value = pickerDate(date);
        }
    }
    syncPickerLimits();
    updateCustomRangePreview();
    rangeCalendar?.sync();
}

export function initCustomTimePicker() {
    const picker = document.getElementById('time-custom');
    rangeCalendar = attachRangeCalendar(document.getElementById('custom-range-calendar'), {
        fromInput: document.getElementById('custom-from'),
        toInput: document.getElementById('custom-to'),
        translate: key => i18n.t(key),
        locale: () => i18n.currentLang,
        isDayAvailable: day => !pickerBoundsLoading && !pickerBoundsError && retainedDay(day),
        onSelect: (first, last) => {
            const endTime = state.collectionIntervalMs < 1000 ? '23:59:59.999' : '23:59:59';
            setPickerDates(clampToRetention(parseDateTimeInput(`${first}T00:00`, state.timeZone)),
                clampToRetention(parseDateTimeInput(`${last}T${endTime}`, state.timeZone)), false);
            picker.querySelectorAll('[data-custom-preset]').forEach(button => button.classList.remove('active'));
        },
    });
    picker.addEventListener('submit', event => {
        event.preventDefault();
        applyCustomRange();
    });
    picker.addEventListener('input', event => {
        if (!['custom-from', 'custom-to'].includes(event.target.id)) return;
        picker.querySelectorAll('[data-custom-preset]').forEach(button => button.classList.remove('active'));
        updateCustomRangePreview();
        rangeCalendar.sync(true);
    });
    picker.addEventListener('keydown', event => {
        if (event.key === 'Enter' && event.target.classList.contains('range-calendar-year')) {
            event.preventDefault();
            event.target.blur();
            return;
        }
        if (event.key !== 'Escape') return;
        event.preventDefault();
        closeCustomTimePicker();
    });
    document.getElementById('btn-cancel-custom').addEventListener('click', () => closeCustomTimePicker());
    // Capture phase also sees clicks on charts/menus that stop propagation.
    document.addEventListener('pointerdown', event => {
        if (!picker.contains(event.target) && !document.getElementById('btn-custom-range').contains(event.target)) {
            closeCustomTimePicker(false);
        }
    }, true);
    document.addEventListener('click', event => {
        if (!picker.contains(event.target) && !document.getElementById('btn-custom-range').contains(event.target)) {
            closeCustomTimePicker(false);
        }
    }, true);
    picker.querySelectorAll('[data-custom-preset]').forEach(button => {
        button.addEventListener('click', () => {
            const now = new Date();
            let to = now;
            let from;
            const preset = button.dataset.customPreset;
            if (preset === 'today' || preset === 'yesterday') {
                from = new Date(now);
                const utc = state.timeZone === 'utc';
                if (utc) from.setUTCHours(0, 0, 0, 0);
                else from.setHours(0, 0, 0, 0);
                if (preset === 'yesterday') {
                    to = new Date(from);
                    if (utc) from.setUTCDate(from.getUTCDate() - 1);
                    else from.setDate(from.getDate() - 1);
                }
            } else {
                from = new Date(now.getTime() - (preset === 'day' ? 86400 : 3600) * 1000);
            }
            setPickerDates(clampToRetention(from), clampToRetention(to));
            picker.querySelectorAll('[data-custom-preset]').forEach(item => item.classList.toggle('active', item === button));
        });
    });
    refreshCustomTimePicker();
}

export function toggleCustomTimePicker() {
    const customEl = document.getElementById('time-custom');
    const isHidden = customEl.classList.contains('hidden');
    if (isHidden) {
        customEl.classList.remove('hidden');
        document.getElementById('btn-custom-range').classList.add('active');
        document.getElementById('btn-custom-range').setAttribute('aria-expanded', 'true');
        const to = state.customTo || new Date(state.historyViewEnd ?? Date.now());
        const from = state.customFrom || new Date(to.getTime() - (state.timeRange || 3600) * 1000);
        customEl.querySelectorAll('[data-custom-preset]').forEach(button => button.classList.remove('active'));
        setPickerDates(clampToRetention(from), clampToRetention(to));
        refreshCustomTimePicker();
        void refreshPickerBounds();
        document.getElementById('custom-from').focus();
    } else {
        closeCustomTimePicker();
    }
}

export function applyCustomRange() {
    if (rangeCalendar && !rangeCalendar.isComplete()) {
        const error = document.getElementById('custom-range-error');
        error.textContent = i18n.t('pick_end_day');
        error.classList.remove('hidden');
        return;
    }
    const { from, to, error } = customRangeDraft();
    updateCustomRangePreview();
    if (error) {
        document.getElementById(!from || (to && from >= to) ? 'custom-from' : 'custom-to').focus();
        return;
    }
    setCustomRange(from, to);
}

export function setCustomRange(fromDate, toDate, { record = true } = {}) {
    state.timeRange = null;
    state.customFrom = new Date(fromDate);
    state.customTo = new Date(toDate);
    closeCustomTimePicker();

    syncCustomRangeUI(state.customFrom, state.customTo);
    updateUrl(state);
    if (record) recordViewport();
    else updateNavigationControls();

    resetZoomAll();
    fetchCustomHistory(state.customFrom, state.customTo);
}

export function commitGestureViewport() {
    if (state.timeRange !== null || !state.customFrom || !state.customTo) return;
    updateUrl(state);
    recordViewport();
}

function applyViewport(viewport) {
    if (!viewport) return;
    if (viewport.kind === 'preset') {
        setTimeRange(viewport.range, { record: false });
    } else {
        setCustomRange(viewport.from, viewport.to, { record: false });
    }
}

export function historyBack() {
    applyViewport(viewportHistory.back());
}

export function historyForward() {
    applyViewport(viewportHistory.forward());
}

export function goLive() {
    setTimeRange(state.lastPresetRange || 300);
}


export function zoomOutViewport() {
    if (!state.customFrom || !state.customTo) return;
    const interval = zoomOutInterval(state.customFrom, state.customTo);
    if (interval) setCustomRange(interval.from, interval.to);
}

// syncCustomRangeUI deselects the preset buttons, marks the custom-range
// button active, and updates the range display label for a custom window.
// It also populates the custom date inputs so they match the active range.
// Shared by applyCustomRange and the URL-state restore on page load.
export function syncCustomRangeUI(fromDate, toDate, { preserveDraft = false } = {}) {
    document.querySelectorAll('.time-btn[data-range]').forEach(b => b.classList.remove('active'));
    document.getElementById('btn-custom-range')?.classList.add('active');

    const fromInput = document.getElementById('custom-from');
    const toInput = document.getElementById('custom-to');
    if (!preserveDraft) {
        if (fromInput) fromInput.value = pickerDate(fromDate);
        if (toInput) toInput.value = pickerDate(toDate);
    }

    const fmt = date => formatRangeTimestamp(date, state.timeZone, i18n.currentLang);
    const zone = state.timeZone === 'utc' ? i18n.t('time_zone_utc') : i18n.t('time_zone_local');
    const display = document.getElementById('time-range-display');
    display?.removeAttribute('data-i18n');
    if (display) display.textContent = `${fmt(fromDate)} → ${fmt(toDate)} · ${zone}`;
}

export function toLocalISOString(date) {
    return formatDateTimeInput(date, 'local');
}
