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
import { attachClockPicker } from './clock-picker.js';
import { isTvModeActive } from './tv-mode.js';
import {
    existingDateTimeInput,
    formatDateTimeInput,
    formatRangeTimestamp,
    formatTimeOfDay,
    parseDateTimeInput,
    parseTimeOfDay,
    stepTimeOfDay,
    timeOfDayMs,
    timeOfDayParts,
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
    // A wall display's resting cursor must not freeze TV mode.
    const pausedHover = state.pausedHover && !isTvModeActive();
    const shouldPause = state.pausedManual || pausedHover || state.pausedZoom;
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
    // Every caller passes a preset (URL ranges are validated against the same
    // list), so the plain duration only guards a preset added without a label.
    if (display) display.textContent = labels[seconds] || `${seconds}s`;
}

// ---- Custom Time Range ----
const MAX_CUSTOM_RANGE_MS = 31 * 86400000;
const ENDPOINTS = {
    start: { field: 'custom-from-field', date: 'custom-from-date', day: 'custom-from-day', time: 'custom-from-time' },
    end: { field: 'custom-to-field', date: 'custom-to-date', day: 'custom-to-day', time: 'custom-to-time' },
};
const PRESET_SECONDS = { '1h': 3600, '6h': 21600, '24h': 86400, '7d': 604800 };
let rangeCalendar;
let clockPicker;
// The calendar pane shows unless a time field is being edited: then the clock
// for that end (clockEndpoint) takes its place.
let clockEndpoint = null;
let calendarEndpoint = 'start';
let pickerBoundsLoading = false;
let pickerBoundsError = false;
let pickerBoundsRequest = 0;
// A time the user typed survives later calendar clicks; untouched ends
// default to whole days.
const typedTimes = { start: false, end: false };

const subSecondPicker = () => state.collectionIntervalMs < 1000;
const endOfDay = () => subSecondPicker() ? '23:59:59.999' : '23:59:59';
const endpointElement = (which, part) => document.getElementById(ENDPOINTS[which][part]);

function pickerDate(date) {
    return formatDateTimeInput(date, state.timeZone, true, subSecondPicker());
}

function retainedRanges() {
    const quantum = subSecondPicker() ? 1 : 1000;
    return (state.retainedRanges || []).map(range => ({
        from: Math.ceil(Date.parse(range.from) / quantum) * quantum,
        to: Math.floor(Math.min(Date.parse(range.to), Date.now()) / quantum) * quantum,
    })).filter(range => Number.isFinite(range.from) && Number.isFinite(range.to) && range.from <= range.to);
}

// Retention bounds which days can be chosen, never the times within them: a
// day with any retained history can be viewed from midnight to midnight.
function retainedDay(day) {
    return retainedRanges().some(range => day >= pickerDate(range.from).slice(0, 10) &&
        day <= pickerDate(range.to).slice(0, 10));
}

function retainedDayBounds() {
    const ranges = retainedRanges();
    if (ranges.length === 0) return null;
    return {
        first: pickerDate(Math.min(...ranges.map(range => range.from))).slice(0, 10),
        last: pickerDate(Math.max(...ranges.map(range => range.to))).slice(0, 10),
    };
}

// Time fields are text in the UI language's clock (see formatTimeOfDay); the
// 24-hour value lives in data-time. Reformat them when the language changes,
// unless the user is typing in one.
function syncTimeInputs() {
    const lang = i18n.currentLang;
    const sample = timeOfDayParts('16:00:00', lang);
    const placeholder = sample.map(part => ({ hour: 'hh', minute: 'mm', second: 'ss' })[part.type] ?? part.value).join('');
    const widest = formatTimeOfDay(subSecondPicker() ? '23:59:59.999' : '23:59:59', lang);
    for (const which of Object.keys(ENDPOINTS)) {
        const input = endpointElement(which, 'time');
        input.placeholder = placeholder;
        input.size = Math.max(placeholder.length, widest.length) + 1;
        // A numeric keypad suffices unless the clock needs a day period.
        input.inputMode = sample.some(part => part.type === 'dayPeriod') ? 'text' : 'decimal';
        if (input.dataset.time && document.activeElement !== input) {
            input.value = formatTimeOfDay(input.dataset.time, lang);
        }
    }
}

function showEndpointTime(which, time) {
    const input = endpointElement(which, 'time');
    input.dataset.time = time;
    input.value = formatTimeOfDay(time, i18n.currentLang);
}

async function refreshPickerBounds() {
    const request = ++pickerBoundsRequest;
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
        syncTimeInputs();
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

function renderEndpointDay(which) {
    const button = endpointElement(which, 'date');
    const day = Date.parse(`${button.dataset.day}T00:00:00Z`);
    const format = options => new Intl.DateTimeFormat(i18n.currentLang, { ...options, timeZone: 'UTC' }).format(day);
    endpointElement(which, 'day').textContent = Number.isFinite(day)
        ? format({ day: 'numeric', month: 'short', year: 'numeric' }) : '—';
    button.title = Number.isFinite(day) ? format({ dateStyle: 'full' }) : '';
}

// Each end of the draft is a calendar day (on its date button) plus a time.
function setEndpoint(which, day, time) {
    endpointElement(which, 'date').dataset.day = day;
    showEndpointTime(which, time);
    renderEndpointDay(which);
}

function endpointValue(which) {
    const day = endpointElement(which, 'date').dataset.day;
    const time = endpointElement(which, 'time').dataset.time;
    return day && time ? `${day}T${time}` : '';
}

// The field of the end being edited is highlighted. A pressed date button
// means the calendar's next click sets that end.
function markActiveEndpoint() {
    const active = clockEndpoint ?? calendarEndpoint;
    for (const which of Object.keys(ENDPOINTS)) {
        endpointElement(which, 'field').classList.toggle('active', which === active);
        endpointElement(which, 'date').setAttribute('aria-pressed', String(!clockEndpoint && which === active));
    }
}

function showPane(which) {
    clockEndpoint = which;
    document.getElementById('custom-range-calendar').classList.toggle('pane-hidden', which !== null);
    document.getElementById('custom-range-clock').classList.toggle('pane-hidden', which === null);
    markActiveEndpoint();
    if (which) clockPicker.update({ reset: true });
}

function clockView() {
    const which = clockEndpoint;
    const day = endpointElement(which, 'date').dataset.day;
    const date = Date.parse(`${day}T00:00:00Z`);
    const dayText = Number.isFinite(date) ? new Intl.DateTimeFormat(i18n.currentLang, {
        weekday: 'short', day: 'numeric', month: 'short', timeZone: 'UTC',
    }).format(date) : '—';
    return {
        title: `${i18n.t(which === 'start' ? 'from' : 'to')} · ${dayText}`,
        time: endpointElement(which, 'time').dataset.time,
        shortcuts: which === 'start' ? ['start_of_day'] : ['end_of_day', 'now'],
    };
}

function writePickerDates(from, to) {
    for (const [which, date] of [['start', from], ['end', to]]) {
        const value = pickerDate(date);
        setEndpoint(which, value.slice(0, 10), value.slice(11));
    }
}

function setPickerDates(from, to) {
    syncTimeInputs();
    writePickerDates(from, to);
    typedTimes.start = false;
    typedTimes.end = false;
    updateCustomRangePreview();
    rangeCalendar?.sync({ reset: true });
}

// Calendar days cover whole days, except that today ends now rather than
// leaving the rest of the chart empty.
function defaultTime(which, day) {
    if (which === 'start') return '00:00:00';
    const now = pickerDate(new Date());
    return now.slice(0, 10) === day ? now.slice(11) : endOfDay();
}

function customPresetRange(preset) {
    const now = new Date();
    if (PRESET_SECONDS[preset]) return { from: new Date(now.getTime() - PRESET_SECONDS[preset] * 1000), to: now };
    const today = pickerDate(now).slice(0, 10);
    if (preset === 'today') return { from: parseDateTimeInput(`${today}T00:00`, state.timeZone), to: now };
    const yesterday = new Date(Date.parse(`${today}T00:00:00Z`) - 86400000).toISOString().slice(0, 10);
    return {
        from: parseDateTimeInput(`${yesterday}T00:00`, state.timeZone),
        to: parseDateTimeInput(`${yesterday}T${endOfDay()}`, state.timeZone),
    };
}

function customRangeDraft() {
    const from = parseDateTimeInput(endpointValue('start'), state.timeZone);
    const to = parseDateTimeInput(endpointValue('end'), state.timeZone);
    const ranges = retainedRanges();
    // Compare days, not instants, so hours before the first sample or after
    // the last one remain selectable.
    const retained = () => ranges.some(range =>
        pickerDate(from).slice(0, 10) <= pickerDate(range.to).slice(0, 10) &&
        pickerDate(to).slice(0, 10) >= pickerDate(range.from).slice(0, 10));
    const unreadable = Object.keys(ENDPOINTS).some(which => {
        const input = endpointElement(which, 'time');
        return input.value.trim() && !input.dataset.time;
    });
    const error = unreadable ? 'range_time_invalid'
        : !from || !to ? 'range_dates_required'
        : from >= to ? 'range_start_before_end'
        : to - from > MAX_CUSTOM_RANGE_MS ? 'range_max_31_days'
        : !subSecondPicker() && (from.getMilliseconds() || to.getMilliseconds()) ? 'range_whole_seconds'
        : pickerBoundsLoading ? 'history_loading'
        : pickerBoundsError ? 'history_failed'
        : ranges.length === 0 ? 'history_empty'
        : !retained() ? 'range_outside_retention' : null;
    return { from, to, error };
}

// A local time the clock skips when daylight saving time begins does not
// exist, and loads as the time an hour later. Show that time instead, except
// in a field being typed in, which catches up when it loses focus.
function showExistingTimes() {
    if (state.timeZone === 'utc') return;
    for (const which of Object.keys(ENDPOINTS)) {
        const value = endpointValue(which);
        if (!value || document.activeElement === endpointElement(which, 'time')) continue;
        const existing = existingDateTimeInput(value, state.timeZone, subSecondPicker());
        if (existing && (existing.slice(0, 10) !== value.slice(0, 10) ||
            timeOfDayMs(existing.slice(11)) !== timeOfDayMs(value.slice(11)))) {
            setEndpoint(which, existing.slice(0, 10), existing.slice(11));
        }
    }
}

function updateCustomRangePreview() {
    showExistingTimes();
    if (clockEndpoint) clockPicker.update();
    const draft = customRangeDraft();
    document.getElementById('btn-apply-custom').disabled = !!draft.error;
    const error = document.getElementById('custom-range-error');
    error.textContent = draft.error === 'range_time_invalid'
        ? `${i18n.t(draft.error)} ${formatTimeOfDay('14:30:00', i18n.currentLang)}`
        : draft.error ? i18n.t(draft.error) : '';
    error.classList.toggle('hidden', !draft.error);
    const inputError = ['range_dates_required', 'range_start_before_end', 'range_max_31_days',
        'range_whole_seconds', 'range_outside_retention'].includes(draft.error);
    for (const [which, date] of [['start', draft.from], ['end', draft.to]]) {
        const invalid = !date || inputError;
        endpointElement(which, 'time').setAttribute('aria-invalid', String(invalid));
        endpointElement(which, 'field').classList.toggle('invalid', invalid);
    }
    const summary = document.getElementById('custom-range-summary');
    if (draft.error) {
        summary.textContent = '';
        return;
    }
    // Below a minute, count seconds rather than showing an empty duration.
    const seconds = (draft.to - draft.from) / 1000;
    let remaining = seconds < 60 ? Math.max(1, Math.round(seconds)) : Math.round(seconds / 60);
    const units = seconds < 60 ? [['second', 1]] : [['day', 1440], ['hour', 60], ['minute', 1]];
    const parts = [];
    for (const [unit, size] of units) {
        const value = Math.floor(remaining / size);
        remaining %= size;
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
    for (const which of Object.keys(ENDPOINTS)) renderEndpointDay(which);
    syncTimeInputs();
    if (document.getElementById('time-custom')?.classList.contains('hidden')) return;
    if (previousZone !== state.timeZone) {
        for (const which of Object.keys(ENDPOINTS)) {
            const date = parseDateTimeInput(endpointValue(which), previousZone);
            if (!date) continue;
            const value = pickerDate(date);
            setEndpoint(which, value.slice(0, 10), value.slice(11));
        }
    }
    updateCustomRangePreview();
    rangeCalendar?.sync();
}

export function initCustomTimePicker() {
    const picker = document.getElementById('time-custom');
    const presets = picker.querySelectorAll('[data-custom-preset]');
    const markPreset = active => presets.forEach(button => button.classList.toggle('active', button === active));
    rangeCalendar = attachRangeCalendar(document.getElementById('custom-range-calendar'), {
        getRange: () => ({
            start: endpointElement('start', 'date').dataset.day,
            end: endpointElement('end', 'date').dataset.day,
        }),
        translate: key => i18n.t(key),
        locale: () => i18n.currentLang,
        today: () => pickerDate(new Date()).slice(0, 10),
        isDayAvailable: day => !pickerBoundsLoading && !pickerBoundsError && retainedDay(day),
        dataBounds: () => pickerBoundsLoading || pickerBoundsError ? null : retainedDayBounds(),
        onSelect: (startDay, endDay, changed) => {
            for (const which of changed) {
                const day = which === 'start' ? startDay : endDay;
                const typed = typedTimes[which] && endpointElement(which, 'time').dataset.time;
                setEndpoint(which, day, typed || defaultTime(which, day));
            }
            markPreset(null);
            updateCustomRangePreview();
        },
        onEndpointChange: active => {
            calendarEndpoint = active;
            markActiveEndpoint();
        },
    });
    clockPicker = attachClockPicker(document.getElementById('custom-range-clock'), {
        getState: clockView,
        translate: key => i18n.t(key),
        locale: () => i18n.currentLang,
        onPick: time => {
            showEndpointTime(clockEndpoint, time);
            typedTimes[clockEndpoint] = true;
            markPreset(null);
            updateCustomRangePreview();
        },
        // Shortcuts equal the defaults, so later day clicks keep choosing them.
        onShortcut: shortcut => {
            const which = clockEndpoint;
            if (shortcut === 'now') {
                const now = pickerDate(new Date());
                setEndpoint(which, now.slice(0, 10), now.slice(11));
                rangeCalendar.sync();
            } else {
                showEndpointTime(which, shortcut === 'start_of_day' ? '00:00:00' : endOfDay());
            }
            typedTimes[which] = false;
            markPreset(null);
            updateCustomRangePreview();
        },
        onCalendar: () => {
            const which = clockEndpoint;
            showPane(null);
            rangeCalendar.editEndpoint(which);
        },
    });
    for (const which of Object.keys(ENDPOINTS)) {
        const input = endpointElement(which, 'time');
        const edited = () => {
            typedTimes[which] = true;
            markPreset(null);
            updateCustomRangePreview();
        };
        endpointElement(which, 'date').addEventListener('click', () => {
            showPane(null);
            rangeCalendar.editEndpoint(which);
        });
        input.addEventListener('focus', () => showPane(which));
        input.addEventListener('input', () => {
            input.dataset.time = parseTimeOfDay(input.value, i18n.currentLang) || '';
            edited();
        });
        // Leaving the field tidies what was typed, e.g. "930" becomes 09:30:00,
        // and catches up with a language change made while it had focus.
        for (const type of ['change', 'blur']) {
            input.addEventListener(type, () => {
                if (input.dataset.time) input.value = formatTimeOfDay(input.dataset.time, i18n.currentLang);
                if (type === 'blur') updateCustomRangePreview();
            });
        }
        // Up/Down step the hours, minutes, seconds or day period at the caret.
        input.addEventListener('keydown', event => {
            if (event.key !== 'ArrowUp' && event.key !== 'ArrowDown') return;
            const step = stepTimeOfDay(input.dataset.time, input.selectionStart ?? 0,
                event.key === 'ArrowUp' ? 1 : -1, i18n.currentLang);
            if (!step) return;
            event.preventDefault();
            input.dataset.time = step.value;
            input.value = step.text;
            input.setSelectionRange(step.start, step.end);
            edited();
        });
    }
    picker.addEventListener('submit', event => {
        event.preventDefault();
        applyCustomRange();
    });
    picker.addEventListener('keydown', event => {
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
    presets.forEach(button => {
        button.addEventListener('click', () => {
            const { from, to } = customPresetRange(button.dataset.customPreset);
            showPane(null);
            setPickerDates(from, to);
            markPreset(button);
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
        showPane(null);
        setPickerDates(from, to);
        refreshCustomTimePicker();
        void refreshPickerBounds();
        rangeCalendar.focus();
    } else {
        closeCustomTimePicker();
    }
}

export function applyCustomRange() {
    const { from, to, error } = customRangeDraft();
    updateCustomRangePreview();
    if (error) {
        endpointElement(!from || (to && from >= to) ? 'start' : 'end', 'time').focus();
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

    if (!preserveDraft && endpointElement('start', 'date')) writePickerDates(fromDate, toDate);

    const fmt = date => formatRangeTimestamp(date, state.timeZone, i18n.currentLang);
    const zone = state.timeZone === 'utc' ? i18n.t('time_zone_utc') : i18n.t('time_zone_local');
    const display = document.getElementById('time-range-display');
    display?.removeAttribute('data-i18n');
    if (display) display.textContent = `${fmt(fromDate)} → ${fmt(toDate)} · ${zone}`;
}
