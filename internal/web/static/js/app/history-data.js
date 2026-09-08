/* ============================================================
   history-data.js — Canonical history items, aggregation
   selection, response provenance, and missing-observation gaps.
   ============================================================ */
'use strict';

const KNOWN_FIELDS = new Set(['data', 'min', 'max']);
const hasOwn = (value, key) => Object.prototype.hasOwnProperty.call(value, key);
const historyContext = Symbol('kulaHistoryContext');
const SUMMARY_SECTIONS = ['cpu', 'lavg', 'mem', 'swap', 'net', 'sys'];

function sectionForCard(cardId) {
    // Application card names can also contain generic system terms such as
    // "connections" and "disk". Classify their anchored prefixes first.
    if (/^card-(?:nginx|apache2|pg|postgres|mysql|containers?|custom)-/.test(cardId)) return 'apps';
    if (/cpu-temp|card-cpu$/.test(cardId)) return 'cpu';
    if (cardId.includes('loadavg')) return 'lavg';
    if (cardId.includes('memory')) return 'mem';
    if (cardId.includes('swap')) return 'swap';
    if (/network|pps|connections|split-net/.test(cardId)) return 'net';
    if (/disk-|diskio|diskspace|disktemp/.test(cardId)) return 'disk';
    if (cardId.includes('entropy')) return 'sys';
    if (cardId.includes('process')) return 'proc';
    if (cardId.includes('self')) return 'self';
    if (/gpu|vram/.test(cardId)) return 'gpu';
    if (/psu|battery|power-supply/.test(cardId)) return 'psu';
    return null;
}

export function historySectionsForFocus(focusMode, cardIds) {
    if (!focusMode || !Array.isArray(cardIds) || cardIds.length === 0) return null;
    const sections = new Set(SUMMARY_SECTIONS);
    cardIds.forEach(cardId => {
        const section = sectionForCard(String(cardId));
        if (section) sections.add(section);
    });
    return Array.from(sections).sort();
}

export function aggregationField(selection) {
    return selection === 'avg' ? 'data' : selection;
}

export function normalizeValidAggregations(values) {
    const valid = Array.isArray(values)
        ? [...new Set(values.filter(value => KNOWN_FIELDS.has(value)))]
        : [];
    return valid.includes('data') ? valid : ['data'];
}

export function resolveAggregation(selection, validAggregations) {
    const valid = normalizeValidAggregations(validAggregations);
    const field = aggregationField(selection);
    if (valid.includes(field)) return { selection, field, valid };
    return { selection: 'avg', field: 'data', valid };
}

function finiteTimestamp(value) {
    if (value === null || value === undefined || value === '') return null;
    const timestamp = new Date(value).getTime();
    return Number.isFinite(timestamp) ? timestamp : null;
}

function finiteNonNegative(value) {
    if (value === null || value === undefined || value === '') return null;
    const number = Number(value);
    return Number.isFinite(number) && number >= 0 ? number : null;
}

function responseSource(response) {
    if (!response || Array.isArray(response)) return null;
    const tier = Number(response.tier);
    const resolution = typeof response.resolution === 'string' ? response.resolution : '';
    const sourceResolution = typeof response.source_resolution === 'string'
        ? response.source_resolution
        : resolution;
    return Object.freeze({
        tier: Number.isInteger(tier) && tier >= 0 ? tier : null,
        resolution,
        sourceResolution,
        downsampled: typeof response.downsampled === 'boolean'
            ? response.downsampled
            : !!resolution && !!sourceResolution && resolution !== sourceResolution,
        complete: typeof response.exact_complete === 'boolean'
            ? response.exact_complete
            : (typeof response.complete === 'boolean' ? response.complete : null),
        retentionComplete: typeof response.complete === 'boolean' ? response.complete : null,
        validAggregations: Object.freeze(Array.isArray(response.valid_aggregations)
            ? response.valid_aggregations.filter(value => typeof value === 'string')
            : ['data']),
    });
}

// Every non-gap buffer entry has the same outer shape. Aggregated history
// items already use it; raw history and WebSocket samples are wrapped without
// changing their inner sample object.
export function normalizeHistoryItem(item) {
    if (!item || typeof item !== 'object' || item._gap) return item;

    if (hasOwn(item, 'data')) {
        if (item.ts != null || item.data?.ts == null) return item;
        return { ...item, ts: item.data.ts };
    }

    return { ts: item.ts, data: item };
}

export function historyItemSample(item) {
    if (!item || item._gap) return null;
    return hasOwn(item, 'data') ? item.data : item;
}

export function historyItemTimestamp(item) {
    if (!item) return null;
    return item.ts ?? item.data?.ts ?? null;
}

// Attach compact, non-enumerable response provenance to each canonical item.
// One shared source object is reused for the whole response, while per-bucket
// metadata remains on the item that owns it.
export function annotateHistoryItems(items, response = null) {
    const source = responseSource(response);
    if (!Array.isArray(items)) return [];

    return items.map(raw => {
        const item = normalizeHistoryItem(raw);
        if (!item || item._gap) return item;

        const timestamp = finiteTimestamp(historyItemTimestamp(item));
        const durationNs = finiteNonNegative(item.dur);
        let bucketStart = finiteTimestamp(item.bucket_start);
        let bucketEnd = finiteTimestamp(item.bucket_end);
        if (bucketEnd === null) bucketEnd = timestamp;
        if (bucketStart === null && bucketEnd !== null && durationNs !== null) {
            bucketStart = bucketEnd - durationNs / 1e6;
        }

        const count = finiteNonNegative(item.sample_count);
        const coverage = finiteNonNegative(item.coverage);
        Object.defineProperty(item, historyContext, {
            configurable: true,
            value: Object.freeze({
                timestamp,
                bucketStart,
                bucketEnd,
                durationNs,
                sampleCount: count === null ? null : Math.trunc(count),
                coverage: coverage === null ? null : Math.min(1, coverage),
                source,
            }),
        });
        return item;
    }).filter(Boolean);
}

export function historyItemContext(item) {
    return item?.[historyContext] || null;
}

function finiteTime(value) {
    if (value === null || value === undefined || value === '') return null;
    const timestamp = value instanceof Date ? value.getTime() : new Date(value).getTime();
    return Number.isFinite(timestamp) ? timestamp : null;
}

function itemTimestamp(item) {
    return finiteTime(item?.ts ?? item?.data?.ts);
}

function resolutionMilliseconds(value) {
    if (typeof value !== 'string') return 1000;
    const match = value.trim().match(/^([\d.]+)\s*(ms|s|m|h|d)$/i);
    if (!match) return 1000;
    const amount = Number(match[1]);
    if (!Number.isFinite(amount) || amount <= 0) return 1000;
    const units = { ms: 1, s: 1000, m: 60000, h: 3600000, d: 86400000 };
    return amount * units[match[2].toLowerCase()];
}

/**
 * Insert explicit missing-observation markers. The marker remains present
 * when Chart.js spanGaps is enabled: that preference controls line joining,
 * so the canonical buffer retains the missing interval.
 */
export function insertHistoryGaps(items, resolution = '1s') {
    if (!Array.isArray(items) || items.length < 2) return Array.isArray(items) ? items.slice() : [];

    let expectedInterval = resolutionMilliseconds(resolution);
    const sampleLimit = Math.min(items.length - 1, 20);
    if (sampleLimit >= 3) {
        const intervals = [];
        for (let index = 0; index < sampleLimit; index++) {
            const current = itemTimestamp(items[index]);
            const next = itemTimestamp(items[index + 1]);
            if (current !== null && next !== null && next > current) intervals.push(next - current);
        }
        intervals.sort((a, b) => a - b);
        const median = intervals[Math.floor(intervals.length / 2)];
        if (Number.isFinite(median) && median > expectedInterval) expectedInterval = median;
    }

    const threshold = expectedInterval * 2.5;
    const result = [];
    for (let index = 0; index < items.length; index++) {
        const item = items[index];
        result.push(item);
        if (index >= items.length - 1 || item?._gap) continue;

        const current = itemTimestamp(item);
        const next = itemTimestamp(items[index + 1]);
        if (current === null || next === null || next - current <= threshold) continue;

        const gapStart = current + expectedInterval;
        result.push({
            _gap: true,
            ts: new Date(gapStart).toISOString(),
            gap_start: new Date(gapStart).toISOString(),
            gap_end: new Date(next).toISOString(),
            observed_from: new Date(current).toISOString(),
            observed_to: new Date(next).toISOString(),
        });
    }
    return result;
}
