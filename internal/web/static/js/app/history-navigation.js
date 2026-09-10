/* ============================================================
   history-navigation.js — Shareable URL state, bounded
   in-dashboard navigation, and deterministic zoom-out ranges.
   ============================================================ */
'use strict';

const VALID_AGG = ['avg', 'min', 'max'];
export const MIN_ZOOM_POINTS = 12;

export function minimumZoomSpan(sourceResolution, collectionIntervalMs = 1000) {
    const units = { ms: 1, s: 1000, m: 60000, h: 3600000 };
    const match = String(sourceResolution || '').match(/^([\d.]+)(ms|s|m|h)$/);
    const source = match ? Number(match[1]) * units[match[2]] : 0;
    const interval = Number.isFinite(collectionIntervalMs) && collectionIntervalMs > 0
        ? collectionIntervalMs : 1000;
    return MIN_ZOOM_POINTS * Math.max(Number.isFinite(source) ? source : 0, interval);
}

// History buffers can contain outages, so a nominal twelve-interval span alone
// is insufficient. Expand toward the nearest retained observations; null
// means the current view has too few observations to zoom in any further.
export function fitZoomToObservations(range, timestamps) {
    const points = [...new Set(timestamps.filter(Number.isFinite))].sort((a, b) => a - b);
    if (points.length < MIN_ZOOM_POINTS) return null;
    if (points.filter(ts => ts >= range.min && ts <= range.max).length >= MIN_ZOOM_POINTS) return range;
    const center = (range.min + range.max) / 2;
    let start = 0;
    while (start + MIN_ZOOM_POINTS < points.length &&
        Math.abs(points[start + MIN_ZOOM_POINTS] - center) < Math.abs(points[start] - center)) start++;
    return {
        min: Math.min(range.min, points[start]),
        max: Math.max(range.max, points[start + MIN_ZOOM_POINTS - 1]),
    };
}

// Preset windows in seconds, mirrors the .time-btn[data-range] buttons
// in index.html. Used to reject arbitrary values from the URL.
const VALID_RANGES = [60, 300, 900, 1800, 3600, 10800, 21600, 43200, 86400, 259200, 604800, 2592000];

function aggRelevant(state) {
    return state.validAggregations?.some(field => field !== 'data') === true;
}

// Parse the current URL into application state. The returned descriptor lets
// the entry point synchronize controls without this module importing the DOM
// controller, which keeps the module graph acyclic.
export function applyUrlState(state) {
    const params = new URLSearchParams(window.location.search);

    const agg = params.get('agg');
    if (agg && VALID_AGG.includes(agg)) {
        state.currentAggregation = agg;
        state.aggFromUrl = true;
    }

    const from = params.get('from');
    const to = params.get('to');
    if (from && to) {
        const fromDate = new Date(from);
        const toDate = new Date(to);
        if (!isNaN(fromDate.getTime()) && !isNaN(toDate.getTime()) && fromDate < toDate) {
            state.timeRange = null;
            state.customFrom = fromDate;
            state.customTo = toDate;
            return { kind: 'custom', from: fromDate, to: toDate };
        }
    }

    const range = params.get('range');
    if (range !== null) {
        const seconds = parseInt(range, 10);
        if (VALID_RANGES.includes(seconds)) {
            state.timeRange = seconds;
            state.lastPresetRange = seconds;
            state.customFrom = null;
            state.customTo = null;
            return { kind: 'preset', range: seconds };
        }
    }
    return null;
}

// Rewrite the URL query string to describe the current view using
// replaceState, so in-dashboard navigation owns the browser history behavior.
export function updateUrl(state) {
    const params = new URLSearchParams(window.location.search);

    if (state.timeRange !== null) {
        params.set('range', String(state.timeRange));
        params.delete('from');
        params.delete('to');
    } else if (state.customFrom && state.customTo) {
        params.delete('range');
        params.set('from', state.customFrom.toISOString());
        params.set('to', state.customTo.toISOString());
    }

    if (aggRelevant(state) && state.currentAggregation !== state.defaultAggregation) {
        params.set('agg', state.currentAggregation);
    } else {
        params.delete('agg');
    }

    const qs = params.toString();
    const newUrl = window.location.pathname + (qs ? '?' + qs : '') + window.location.hash;
    window.history.replaceState(window.history.state, '', newUrl);
}

function cloneViewport(viewport) {
    if (!viewport) return null;
    if (viewport.kind === 'preset') {
        return { kind: 'preset', range: Number(viewport.range) };
    }
    return {
        kind: 'custom',
        from: new Date(viewport.from),
        to: new Date(viewport.to),
    };
}

function viewportKey(viewport) {
    if (!viewport) return '';
    if (viewport.kind === 'preset') return `preset:${viewport.range}`;
    return `custom:${new Date(viewport.from).toISOString()}:${new Date(viewport.to).toISOString()}`;
}

export class ViewportHistory {
    constructor(initial = null, limit = 50) {
        this.limit = limit;
        this.entries = [];
        this.index = -1;
        if (initial) this.push(initial);
    }

    current() {
        return cloneViewport(this.entries[this.index]);
    }

    push(viewport) {
        const next = cloneViewport(viewport);
        if (!next) return this.current();
        if (viewportKey(this.entries[this.index]) === viewportKey(next)) return this.current();

        this.entries.splice(this.index + 1);
        this.entries.push(next);
        if (this.entries.length > this.limit) this.entries.shift();
        this.index = this.entries.length - 1;
        return this.current();
    }

    back() {
        if (!this.canBack()) return null;
        this.index--;
        return this.current();
    }

    forward() {
        if (!this.canForward()) return null;
        this.index++;
        return this.current();
    }

    canBack() {
        return this.index > 0;
    }

    canForward() {
        return this.index >= 0 && this.index < this.entries.length - 1;
    }
}

export function zoomOutInterval(from, to, factor = 2, now = new Date(), maxSpanMs = 31 * 86400000) {
    const fromMs = new Date(from).getTime();
    const toMs = new Date(to).getTime();
    if (!Number.isFinite(fromMs) || !Number.isFinite(toMs) || toMs <= fromMs) return null;

    const span = (toMs - fromMs) * factor;
    const center = fromMs + (toMs - fromMs) / 2;
    const bounded = clampHistoryInterval(center - span / 2, center + span / 2, now, maxSpanMs);
    return bounded ? { from: new Date(bounded.min), to: new Date(bounded.max) } : null;
}

// Clamp every gesture path to the same public history contract. When a pan
// reaches the future, preserve its duration by shifting the whole window back.
export function clampHistoryInterval(min, max, now = Date.now(), maxSpanMs = 31 * 86400000, minSpanMs = 0) {
    let fromMs = Number(min);
    let toMs = Number(max);
    const nowMs = new Date(now).getTime();
    if (!Number.isFinite(fromMs) || !Number.isFinite(toMs) || toMs <= fromMs ||
        !Number.isFinite(nowMs) || !Number.isFinite(maxSpanMs) || maxSpanMs <= 0) {
        return null;
    }

    const span = Math.min(Math.max(toMs - fromMs, minSpanMs), maxSpanMs);
    const center = fromMs + (toMs - fromMs) / 2;
    fromMs = center - span / 2;
    toMs = center + span / 2;
    if (toMs > nowMs) {
        fromMs -= toMs - nowMs;
        toMs = nowMs;
    }
    return { min: fromMs, max: toMs };
}
