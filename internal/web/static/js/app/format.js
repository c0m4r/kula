/* ============================================================
   format.js — Metric and timezone-aware date/time formatting,
   history tooltip context, and datetime input conversion.
   ============================================================ */
'use strict';

function expandExponent(text) {
    const match = /^(-?)(\d+)(?:\.(\d*))?[eE]([+-]?\d+)$/.exec(text);
    if (!match) return text;
    const [, sign, whole, fraction = '', exponentText] = match;
    const digits = whole + fraction;
    const decimalAt = whole.length + Number(exponentText);
    if (decimalAt <= 0) return `${sign}0.${'0'.repeat(-decimalAt)}${digits}`;
    if (decimalAt >= digits.length) return `${sign}${digits}${'0'.repeat(decimalAt - digits.length)}`;
    return `${sign}${digits.slice(0, decimalAt)}.${digits.slice(decimalAt)}`;
}

/**
 * Format a metric without leaking binary floating-point noise or exponent
 * notation. Ordinary values use at most two decimals; sub-hundredth non-zero
 * rates retain roughly three significant digits, up to fifteen decimals.
 */
export function formatMetricNumber(value) {
    const number = Number(value);
    if (!Number.isFinite(number)) return '—';
    if (number === 0) return '0';

    let precision = 2;
    const absolute = Math.abs(number);
    if (absolute < 0.01) {
        precision = Math.min(15, Math.max(2, Math.ceil(-Math.log10(absolute)) + 2));
    }
    let formatted = expandExponent(number.toFixed(precision));
    if (formatted.includes('.')) {
        formatted = formatted.replace(/0+$/, '').replace(/\.$/, '');
    }
    return formatted === '-0' ? '0' : formatted;
}

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

// Times of day in the custom range picker. Values are 24-hour 'HH:MM:SS' with
// optional '.mmm'; the text uses the UI language's clock, like the header
// clock, rather than the browser's, so Polish shows 16:57:33 and English
// 04:57:33 PM even when the browser is set up otherwise.
const DAY_MS = 86400000;
const TIME_STEPS = { hour: 3600000, minute: 60000, second: 1000, fractionalSecond: 1, dayPeriod: 43200000 };

export function timeOfDayMs(value) {
    const match = /^(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,3}))?$/.exec(String(value || ''));
    if (!match || Number(match[1]) > 23 || Number(match[2]) > 59 || Number(match[3]) > 59) return null;
    return ((Number(match[1]) * 60 + Number(match[2])) * 60 + Number(match[3])) * 1000 +
        Number((match[4] || '0').padEnd(3, '0'));
}

export function timeOfDayValue(ms) {
    const total = ((Math.round(ms) % DAY_MS) + DAY_MS) % DAY_MS;
    const pad = (number, width = 2) => String(number).padStart(width, '0');
    const seconds = Math.floor(total / 1000);
    return `${pad(Math.floor(seconds / 3600))}:${pad(Math.floor(seconds / 60) % 60)}:${pad(seconds % 60)}` +
        (total % 1000 ? `.${pad(total % 1000, 3)}` : '');
}

export function timeOfDayParts(value, locale) {
    const ms = timeOfDayMs(value);
    if (ms === null) return [];
    return formatter(locale, 'utc', {
        hour: '2-digit', minute: '2-digit', second: '2-digit', ...(ms % 1000 ? { fractionalSecondDigits: 3 } : {}),
    }).formatToParts(ms);
}

export function formatTimeOfDay(value, locale) {
    return timeOfDayParts(value, locale).map(part => part.value).join('');
}

// Reads the formatted text back, and forgiving typing such as "14:30", "1430",
// "2:30 pm" or the language's own digits and day periods. Missing minutes and
// seconds are zero; a time without a day period is read as 24-hour.
export function parseTimeOfDay(text, locale) {
    const periods = { am: new Set(['am', 'a']), pm: new Set(['pm', 'p']) };
    for (const [period, hour] of [['am', 4], ['pm', 16]]) {
        const name = formatter(locale, 'utc', { hour: 'numeric', hour12: true })
            .formatToParts(Date.UTC(2000, 0, 1, hour)).find(part => part.type === 'dayPeriod')?.value;
        if (name) periods[period].add(name.toLowerCase().replace(/[\s.]/g, ''));
    }
    const digits = new Intl.NumberFormat(locale || undefined, { useGrouping: false }).format(1234567890);
    const normalized = [...String(text || '')]
        .map(character => digits.includes(character) ? '1234567890'[digits.indexOf(character)] : character)
        .join('').toLowerCase();
    const word = (normalized.match(/\p{L}+/gu) || []).join('');
    const period = !word ? null : periods.pm.has(word) ? 'pm' : periods.am.has(word) ? 'am' : undefined;
    const groups = normalized.match(/\d+/g) || [];
    if (period === undefined || groups.length === 0 || groups.length > 4) return null;
    let [hour, minute = '0', second = '0'] = groups;
    const fraction = groups[3] || '';
    if (groups.length === 1 && hour.length > 2 && hour.length <= 6) {
        const rest = hour.length > 4 ? 4 : 2;
        [hour, minute, second] = [hour.slice(0, -rest), hour.slice(-rest, hour.length - rest + 2),
            rest === 4 ? hour.slice(-2) : '0'];
    }
    if (hour.length > 2 || minute.length > 2 || second.length > 2 || fraction.length > 3 ||
        Number(minute) > 59 || Number(second) > 59) return null;
    let hours = Number(hour);
    if (period ? hours > 12 : hours > 23) return null;
    if (period) hours = hours % 12 + (period === 'pm' ? 12 : 0);
    return timeOfDayValue(((hours * 60 + Number(minute)) * 60 + Number(second)) * 1000 +
        Number(fraction.padEnd(3, '0')));
}

function timeOfDayRanges(value, locale) {
    const ranges = [];
    let offset = 0;
    for (const part of timeOfDayParts(value, locale)) {
        if (TIME_STEPS[part.type]) ranges.push({ type: part.type, start: offset, end: offset + part.value.length });
        offset += part.value.length;
    }
    return ranges;
}

// Steps the part of the formatted text at caret (hours, minutes, seconds or
// the day period) by one, wrapping within the day. Returns the new value, its
// text and that part's range in it for selection.
export function stepTimeOfDay(value, caret, direction, locale) {
    const ms = timeOfDayMs(value);
    const ranges = timeOfDayRanges(value, locale);
    if (ms === null || ranges.length === 0) return null;
    const range = ranges.find(item => caret >= item.start && caret <= item.end) ||
        ranges.filter(item => item.end < caret).at(-1) || ranges[0];
    const next = timeOfDayValue(ms + Math.sign(direction) * TIME_STEPS[range.type]);
    const nextRanges = timeOfDayRanges(next, locale);
    const selection = nextRanges.find(item => item.type === range.type) ||
        nextRanges.find(item => item.type === 'second') || nextRanges[0];
    return { value: next, text: formatTimeOfDay(next, locale), start: selection.start, end: selection.end };
}

// Labels for the picker's clock dial and wheels in the UI language's clock:
// its digits and hour cycle, and on a 12-hour clock its two day-period names
// and whether they come first. `hours` holds 0–23 in order, `sixty` 00–59, and
// `separator` the mark between hours and minutes.
export function clockFace(locale) {
    const part = (parts, type) => parts.find(item => item.type === type)?.value ?? '';
    const sample = timeOfDayParts('16:05:09', locale);
    const types = sample.map(item => item.type);
    const twelve = types.includes('dayPeriod');
    const named = formatter(locale, 'utc', { hour: 'numeric', minute: '2-digit' });
    const hourOnly = formatter(locale, 'utc', { hour: twelve ? 'numeric' : '2-digit' });
    return {
        // The time field's own words: locale data differs between runtimes
        // (Korean is "AM" in some and "오전" in others), and the two must match.
        periods: twelve ? ['04:00:00', '16:00:00'].map(time => part(timeOfDayParts(time, locale), 'dayPeriod')) : null,
        periodFirst: twelve && types.indexOf('dayPeriod') < types.indexOf('hour'),
        hours: Array.from({ length: 24 }, (_, hour) => part(hourOnly.formatToParts(hour * 3600000), 'hour')),
        sixty: Array.from({ length: 60 }, (_, minute) =>
            part(timeOfDayParts(timeOfDayValue(minute * 60000), locale), 'minute')),
        separator: sample[types.indexOf('minute') - 1]?.value ?? ':',
        // An accessible name such as "2:05 PM" for a time of day in ms.
        name: ms => named.format(ms),
    };
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
    extremaSources = [],
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

    const valid = Array.isArray(source?.availableAggregations) ? source.availableAggregations :
        (Array.isArray(source?.validAggregations) ? source.validAggregations : ['data']);
    const extremaOffered = valid.includes('min') && valid.includes('max');
    const hasLegacy = extremaSources.some(value => value === 'legacy' || value === 'mixed');
    const hasRange = extremaOffered &&
        (extremaSources.length === 0 || extremaSources.some(value => value !== 'unavailable'));
    if (aggregation === 'min' && valid.includes('min')) {
        lines.push(`${translate('representative')}: ${translate('bucket_minimum')}`);
    } else if (aggregation === 'max' && valid.includes('max')) {
        lines.push(`${translate('representative')}: ${translate('bucket_maximum')}`);
    } else if (hasRange && !hasLegacy) {
        lines.push(`${translate('representative')}: ${translate('policy_center')}`);
    } else {
        lines.push(`${translate('representative')}: ${translate('raw_or_stored_value')}`);
    }
    if (hasRange) {
        lines.push(`${translate('range_band')}: ${translate('bucket_minimum')}–${translate('bucket_maximum')}`);
    }
    // Raw windows do not offer Min/Max at all, so their missing envelopes are
    // not a limitation worth reporting.
    if (extremaOffered && extremaSources.includes('unavailable')) {
        lines.push(translate('history_extrema_unavailable'));
    }
    if (hasLegacy) {
        lines.push(translate('history_legacy_extrema'));
    }
    return lines;
}
