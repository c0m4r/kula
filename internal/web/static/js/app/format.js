/* ============================================================
   format.js — Metric and timezone-aware date/time formatting,
   history tooltip context, and datetime input conversion.
   ============================================================ */
'use strict';

export function formatBytesShort(bytes) {
    if (bytes === 0 || bytes === undefined || bytes === null || isNaN(bytes)) return '0 B';
    if (Math.abs(bytes) < 1) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    const i = Math.floor(Math.log(Math.abs(bytes)) / Math.log(1024));
    const idx = Math.max(0, Math.min(i, units.length - 1));
    return (bytes / Math.pow(1024, idx)).toFixed(idx > 0 ? 1 : 0) + ' ' + units[idx];
}

export function formatMbps(v) {
    if (v < 1) return (v * 1000).toFixed(0) + ' Kbps';
    return v.toFixed(2) + ' Mbps';
}

export function formatPPS(v) {
    if (v === undefined || v === null || isNaN(v)) return '0 pps';
    if (v >= 1000000) return (v / 1000000).toFixed(1) + ' Mpps';
    if (v >= 1000) return (v / 1000).toFixed(1) + ' Kpps';
    return Math.round(v) + ' pps';
}

export function normalizeTimeZone(value) {
    return value === 'utc' ? 'utc' : 'local';
}

// Intl formatter construction dominates chart tick generation when done per
// label. Bound the cache and share it across all charts and tooltip formats.
const formatters = new Map();
function formatter(locale, mode, options) {
    const timeZone = normalizeTimeZone(mode) === 'utc' ? { timeZone: 'UTC' } : {};
    const key = JSON.stringify([locale || null, timeZone, options]);
    if (!formatters.has(key)) {
        if (formatters.size >= 64) formatters.delete(formatters.keys().next().value);
        formatters.set(key, new Intl.DateTimeFormat(locale || undefined, { ...options, ...timeZone }));
    }
    return formatters.get(key);
}

function validDate(value) {
    if (value === null || value === undefined || value === '') return null;
    const date = value instanceof Date ? value : new Date(value);
    return Number.isFinite(date.getTime()) ? date : null;
}

export function formatFullTimestamp(value, mode = 'local', locale) {
    const date = validDate(value);
    if (!date) return '';
    return formatter(locale, mode, {
        year: 'numeric',
        month: 'short',
        day: 'numeric',
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
        timeZoneName: 'short',
    }).format(date);
}

export function formatRangeTimestamp(value, mode = 'local', locale) {
    const date = validDate(value);
    if (!date) return '';
    return formatter(locale, mode, {
        year: 'numeric',
        month: 'short',
        day: 'numeric',
        hour: '2-digit',
        minute: '2-digit',
    }).format(date);
}

export function formatClockTimestamp(value, mode = 'local', locale) {
    const date = validDate(value);
    if (!date) return '';
    return formatter(locale, mode, {
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
    }).format(date);
}

export function formatChartTick(value, mode = 'local', unit = '', locale) {
    const date = validDate(value);
    if (!date) return '';
    let options;
    switch (unit) {
    case 'year':
        options = { year: 'numeric' };
        break;
    case 'quarter':
    case 'month':
        options = { month: 'short', year: 'numeric' };
        break;
    case 'week':
    case 'day':
        options = { month: 'short', day: 'numeric' };
        break;
    case 'millisecond':
    case 'second':
        options = { hour: '2-digit', minute: '2-digit', second: '2-digit' };
        break;
    default:
        options = { hour: '2-digit', minute: '2-digit' };
        break;
    }
    return formatter(locale, mode, options).format(date);
}

export function formatDateTimeInput(value, mode = 'local', includeSeconds = false, includeMilliseconds = false) {
    const date = validDate(value);
    if (!date) return '';
    const utc = normalizeTimeZone(mode) === 'utc';
    const part = name => date[`${utc ? 'getUTC' : 'get'}${name}`]();
    const pad = number => String(number).padStart(2, '0');
    const seconds = includeSeconds ? `:${pad(part('Seconds'))}` +
        (includeMilliseconds && part('Milliseconds') ? `.${String(part('Milliseconds')).padStart(3, '0')}` : '') : '';
    return `${part('FullYear')}-${pad(part('Month') + 1)}-${pad(part('Date'))}` +
        `T${pad(part('Hours'))}:${pad(part('Minutes'))}${seconds}`;
}

export function parseDateTimeInput(value, mode = 'local') {
    const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2})(?:\.(\d{1,3}))?)?$/.exec(String(value || ''));
    if (!match) return null;
    const [, year, month, day, hour, minute, second = '0', millis = '0'] = match;
    const args = [Number(year), Number(month) - 1, Number(day), Number(hour),
        Number(minute), Number(second), Number(millis.padEnd(3, '0'))];
    const date = normalizeTimeZone(mode) === 'utc'
        ? new Date(Date.UTC(...args))
        : new Date(...args);
    return Number.isFinite(date.getTime()) ? date : null;
}

function percentage(value) {
    const percent = Math.max(0, Math.min(1, Number(value))) * 100;
    return `${percent.toFixed(percent >= 99.95 || percent === 0 ? 0 : 1)}%`;
}

// Chart.js accepts a string array from a tooltip footer callback. Keep these
// lines factual: sampleCount is explicitly a count of selected-tier source
// records, while coverage is derived from observed duration / bucket width.
export function historyTooltipLines(context, {
    mode = 'local',
    locale,
    aggregation = 'avg',
    translate = key => key,
} = {}) {
    if (!context) return [];
    const lines = [];
    if (Number.isFinite(context.bucketStart) && Number.isFinite(context.bucketEnd)) {
        lines.push(`${translate('bucket_start')}: ${formatFullTimestamp(context.bucketStart, mode, locale)}`);
        lines.push(`${translate('bucket_end')}: ${formatFullTimestamp(context.bucketEnd, mode, locale)}`);
    }

    const source = context.source;
    if (source && (source.tier !== null || source.sourceResolution || source.resolution)) {
        const tier = source.tier === null ? '' : `${translate('tier')} ${source.tier}`;
        const sourceResolution = source.sourceResolution || source.resolution;
        lines.push(`${translate('source')}: ${[tier, sourceResolution].filter(Boolean).join(' · ')}`);
        if (source.resolution && source.resolution !== sourceResolution) {
            lines.push(`${translate('output_resolution')}: ${source.resolution}`);
        }
    }
    if (Number.isFinite(context.sampleCount) && context.sampleCount > 0) {
        lines.push(`${translate('contributors')}: ${context.sampleCount} ${translate('source_records')}`);
    }

    const coverage = Number.isFinite(context.coverage) ? percentage(context.coverage) : null;
    const rangeState = source?.complete === false
        ? translate('partial_range')
        : source?.complete === true ? translate('complete_range') : null;
    if (coverage || rangeState) {
        lines.push(`${translate('coverage')}: ${[coverage, rangeState].filter(Boolean).join(' · ')}`);
    }

    const valid = Array.isArray(source?.validAggregations) ? source.validAggregations : ['data'];
    const hasRange = valid.includes('min') && valid.includes('max');
    if (aggregation === 'min' && valid.includes('min')) {
        lines.push(`${translate('representative')}: ${translate('bucket_minimum')}`);
    } else if (aggregation === 'max' && valid.includes('max')) {
        lines.push(`${translate('representative')}: ${translate('bucket_maximum')}`);
    } else if (hasRange) {
        lines.push(`${translate('representative')}: ${translate('policy_center')}`);
    } else {
        lines.push(`${translate('representative')}: ${translate('raw_or_stored_value')}`);
    }
    if (hasRange) {
        lines.push(`${translate('range_band')}: ${translate('bucket_minimum')}–${translate('bucket_maximum')}`);
    }
    return lines;
}
