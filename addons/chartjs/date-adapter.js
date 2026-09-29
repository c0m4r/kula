/* ============================================================
   date-adapter.js — Chart.js time-scale adapter on native Date.
   Same local-time calendar math as chartjs-adapter-date-fns, without
   date-fns. Kula labels ticks and tooltips with Intl (format.js), so
   format() covers only the tokens of these defaults and Kula's own
   chart options: yyyy qqq MMM MM dd d HH H hh h mm ss SSS a.
   ============================================================ */
'use strict';

const FORMATS = {
    datetime: 'MMM d, yyyy, h:mm:ss a',
    millisecond: 'h:mm:ss.SSS a',
    second: 'h:mm:ss a',
    minute: 'h:mm a',
    hour: 'ha',
    day: 'MMM d',
    week: 'MMM d, yyyy',
    month: 'MMM yyyy',
    quarter: 'qqq - yyyy',
    year: 'yyyy',
};

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
const TOKENS = /yyyy|qqq|MMM|MM|dd|d|HH|H|hh|h|mm|ss|SSS|a/g;
const pad = (value, width = 2) => String(value).padStart(width, '0');

const TOKEN_FORMATTERS = {
    yyyy: date => String(date.getFullYear()),
    qqq: date => `Q${Math.floor(date.getMonth() / 3) + 1}`,
    MMM: date => MONTHS[date.getMonth()],
    MM: date => pad(date.getMonth() + 1),
    dd: date => pad(date.getDate()),
    d: date => String(date.getDate()),
    HH: date => pad(date.getHours()),
    H: date => String(date.getHours()),
    hh: date => pad(date.getHours() % 12 || 12),
    h: date => String(date.getHours() % 12 || 12),
    mm: date => pad(date.getMinutes()),
    ss: date => pad(date.getSeconds()),
    SSS: date => pad(date.getMilliseconds(), 3),
    a: date => (date.getHours() < 12 ? 'AM' : 'PM'),
};

const toDate = time => new Date(time instanceof Date ? time.getTime() : time);

function startOfWeek(date, weekStartsOn) {
    const day = date.getDay();
    date.setDate(date.getDate() - ((day < weekStartsOn ? 7 : 0) + day - weekStartsOn));
    date.setHours(0, 0, 0, 0);
    return date.getTime();
}

// A later date in a shorter month clamps to its last day (Jan 31 + 1 month = Feb 28).
function addMonths(date, amount) {
    if (!amount) return date.getTime();
    const day = date.getDate();
    const end = new Date(date.getTime());
    end.setMonth(date.getMonth() + amount + 1, 0);
    if (day >= end.getDate()) return end.getTime();
    date.setFullYear(end.getFullYear(), end.getMonth(), day);
    return date.getTime();
}

// Whole local days between wall-clock times, so a DST day still counts as one.
function wholeDays(later, earlier) {
    const wallClock = date => Date.UTC(date.getFullYear(), date.getMonth(), date.getDate(),
        date.getHours(), date.getMinutes(), date.getSeconds(), date.getMilliseconds());
    return Math.trunc((wallClock(later) - wallClock(earlier)) / 864e5);
}

// Whole calendar months: adding the result to earlier never passes later.
function wholeMonths(later, earlier) {
    const sign = later < earlier ? -1 : 1;
    let months = (later.getFullYear() - earlier.getFullYear()) * 12 + later.getMonth() - earlier.getMonth();
    if (sign * (addMonths(new Date(earlier.getTime()), months) - later) > 0) months -= sign;
    return months;
}

export const dateAdapter = {
    _id: 'kula-native',

    formats() {
        return FORMATS;
    },

    // Timestamps, Dates and ISO 8601 strings; Kula never sets time.parser.
    parse(value) {
        let time = null;
        if (typeof value === 'number' || value instanceof Date) time = +value;
        else if (typeof value === 'string') time = Date.parse(value);
        return Number.isFinite(time) ? time : null;
    },

    format(time, fmt) {
        const date = toDate(time);
        if (!Number.isFinite(date.getTime())) return '';
        return String(fmt).replace(TOKENS, token => TOKEN_FORMATTERS[token](date));
    },

    add(time, amount, unit) {
        amount = Math.trunc(amount);
        if (!Number.isFinite(amount)) return NaN;
        const date = toDate(time);
        // Re-setting an ambiguous DST wall-clock time could move it an hour.
        if (!amount) return date.getTime();
        switch (unit) {
        case 'millisecond': return date.getTime() + amount;
        case 'second': return date.getTime() + amount * 1000;
        case 'minute': return date.getTime() + amount * 60000;
        case 'hour': return date.getTime() + amount * 3600000;
        case 'day': return date.setDate(date.getDate() + amount);
        case 'week': return date.setDate(date.getDate() + amount * 7);
        case 'month': return addMonths(date, amount);
        case 'quarter': return addMonths(date, amount * 3);
        case 'year': return addMonths(date, amount * 12);
        default: return time;
        }
    },

    diff(max, min, unit) {
        const later = toDate(max);
        const earlier = toDate(min);
        const ms = later - earlier;
        switch (unit) {
        case 'millisecond': return ms;
        case 'second': return Math.trunc(ms / 1000);
        case 'minute': return Math.trunc(ms / 60000);
        case 'hour': return Math.trunc(ms / 3600000);
        case 'day': return wholeDays(later, earlier);
        case 'week': return Math.trunc(wholeDays(later, earlier) / 7);
        case 'month': return wholeMonths(later, earlier);
        case 'quarter': return Math.trunc(wholeMonths(later, earlier) / 3);
        case 'year': return Math.trunc(wholeMonths(later, earlier) / 12);
        default: return 0;
        }
    },

    startOf(time, unit, weekday) {
        const date = toDate(time);
        switch (unit) {
        case 'second': return date.setMilliseconds(0);
        case 'minute': return date.setSeconds(0, 0);
        case 'hour': return date.setMinutes(0, 0, 0);
        case 'day': return date.setHours(0, 0, 0, 0);
        case 'week': return startOfWeek(date, 0);
        case 'isoWeek': return startOfWeek(date, +weekday);
        case 'month':
            date.setDate(1);
            return date.setHours(0, 0, 0, 0);
        case 'quarter':
            date.setMonth(date.getMonth() - date.getMonth() % 3, 1);
            return date.setHours(0, 0, 0, 0);
        case 'year':
            date.setMonth(0, 1);
            return date.setHours(0, 0, 0, 0);
        default: return time;
        }
    },

    endOf(time, unit) {
        const date = toDate(time);
        switch (unit) {
        case 'second': return date.setMilliseconds(999);
        case 'minute': return date.setSeconds(59, 999);
        case 'hour': return date.setMinutes(59, 59, 999);
        case 'day': return date.setHours(23, 59, 59, 999);
        case 'week':
            date.setDate(date.getDate() + 6 - date.getDay());
            return date.setHours(23, 59, 59, 999);
        case 'month':
            date.setMonth(date.getMonth() + 1, 0);
            return date.setHours(23, 59, 59, 999);
        case 'quarter':
            date.setMonth(date.getMonth() - date.getMonth() % 3 + 3, 0);
            return date.setHours(23, 59, 59, 999);
        case 'year':
            date.setMonth(12, 0);
            return date.setHours(23, 59, 59, 999);
        default: return time;
        }
    },
};
