/* ============================================================
   ai-chart-csv.js — a chart's visible series as the CSV the AI
   panel sends with a chart analysis. Import-free, so the Node
   tests load it directly.
   ============================================================ */
'use strict';

const pad = (n, width = 2) => String(n).padStart(width, '0');

/** Calendar fields of ms in the dashboard's zone: 'utc' or the browser's. */
function fields(ms, timeZone) {
    const d = new Date(ms);
    return timeZone === 'utc'
        ? { y: d.getUTCFullYear(), mo: d.getUTCMonth() + 1, d: d.getUTCDate(), h: d.getUTCHours(), mi: d.getUTCMinutes(), s: d.getUTCSeconds() }
        : { y: d.getFullYear(), mo: d.getMonth() + 1, d: d.getDate(), h: d.getHours(), mi: d.getMinutes(), s: d.getSeconds() };
}

/** "UTC" or the browser's zone name with its offset at ms, e.g. "Europe/Warsaw, UTC+01:00". */
export function zoneLabel(ms, timeZone) {
    if (timeZone === 'utc') return 'UTC';
    const offset = -new Date(ms).getTimezoneOffset();
    const sign = offset < 0 ? '-' : '+';
    const abs = Math.abs(offset);
    const utc = `UTC${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)}`;
    let name = '';
    try {
        name = Intl.DateTimeFormat().resolvedOptions().timeZone || '';
    } catch {
        // No Intl zone data: the offset alone still places the times.
    }
    return name ? `${name}, ${utc}` : utc;
}

/**
 * timeFormatter returns a function writing a time for rows spanning
 * from–to: the date only when the span crosses a day, seconds only when
 * rows are less than a minute apart.
 */
export function timeFormatter(from, to, rowGapMs, timeZone = 'local') {
    const a = fields(from, timeZone);
    const b = fields(to, timeZone);
    const withDate = a.y !== b.y || a.mo !== b.mo || a.d !== b.d;
    const withSeconds = rowGapMs < 60000;
    return (ms) => {
        const f = fields(ms, timeZone);
        let out = `${pad(f.h)}:${pad(f.mi)}`;
        if (withSeconds) out += `:${pad(f.s)}`;
        if (withDate) out = `${f.y}-${pad(f.mo)}-${pad(f.d)} ${out}`;
        return out;
    };
}

function fullTime(ms, timeZone) {
    const f = fields(ms, timeZone);
    return `${f.y}-${pad(f.mo)}-${pad(f.d)} ${pad(f.h)}:${pad(f.mi)}:${pad(f.s)}`;
}

const csvField = (text) => `"${String(text).replace(/"/g, '""')}"`;
const num = (v) => (Number.isInteger(v) ? String(v) : v.toFixed(2));

/** Finite { x, y } points of a series, in time order. */
function cleanPoints(points) {
    const out = [];
    for (const p of points || []) {
        const x = p?.x instanceof Date ? p.x.getTime() : Number(p?.x);
        const y = Number(p?.y);
        if (p && p.y !== null && p.y !== undefined && Number.isFinite(x) && Number.isFinite(y)) out.push({ x, y });
    }
    return out.sort((a, b) => a.x - b.x);
}

/**
 * chartCSV writes series ({ label, points: [{ x: ms, y }] }) as CSV with a
 * comment header naming the range, the zone and what each row holds. At
 * most maxRows rows: when the series have more distinct times, the range
 * is cut into maxRows equal buckets and each series gets the mean and the
 * maximum of its points in each, so a short spike survives. A cell with no
 * point is empty. Returns null when no series has a point.
 */
export function chartCSV(series, { timeZone = 'local', maxRows = 50, notes = [] } = {}) {
    const cols = series
        .map((s) => ({ label: s.label || 'Value', points: cleanPoints(s.points) }))
        .filter((s) => s.points.length > 0);
    if (cols.length === 0) return null;

    let from = Infinity;
    let to = -Infinity;
    const times = new Set();
    for (const c of cols) {
        from = Math.min(from, c.points[0].x);
        to = Math.max(to, c.points[c.points.length - 1].x);
        for (const p of c.points) times.add(p.x);
    }

    const lines = [];
    const header = ['Time'];
    let rows;
    let rowGap;
    if (times.size <= maxRows) {
        // One row per sample time.
        const sorted = [...times].sort((a, b) => a - b);
        rowGap = sorted.length > 1 ? Math.min(...sorted.slice(1).map((t, i) => t - sorted[i])) : Infinity;
        const fmt = timeFormatter(from, to, rowGap, timeZone);
        for (const c of cols) header.push(csvField(c.label));
        const byTime = cols.map((c) => new Map(c.points.map((p) => [p.x, p.y])));
        rows = sorted.map((t) => [fmt(t), ...byTime.map((m) => (m.has(t) ? num(m.get(t)) : ''))].join(','));
        lines.push(`# One row per sample, ${sorted.length} rows`);
    } else {
        const width = (to - from) / maxRows;
        rowGap = width;
        const fmt = timeFormatter(from, to, rowGap, timeZone);
        const buckets = cols.map(() => Array.from({ length: maxRows }, () => ({ sum: 0, n: 0, max: -Infinity })));
        cols.forEach((c, ci) => {
            for (const p of c.points) {
                const b = buckets[ci][Math.min(maxRows - 1, Math.floor((p.x - from) / width))];
                b.sum += p.y;
                b.n++;
                if (p.y > b.max) b.max = p.y;
            }
        });
        for (const c of cols) header.push(csvField(`${c.label} mean`), csvField(`${c.label} max`));
        rows = [];
        for (let i = 0; i < maxRows; i++) {
            const row = [fmt(from + i * width)];
            let any = false;
            for (const col of buckets) {
                const b = col[i];
                if (b.n) any = true;
                row.push(b.n ? num(b.sum / b.n) : '', b.n ? num(b.max) : '');
            }
            if (any) rows.push(row.join(','));
        }
        const seconds = Math.round(width / 1000);
        lines.push(`# Each row: mean and max over ${seconds} s starting at its time, ${rows.length} rows`);
    }
    lines.unshift(`# Range: ${fullTime(from, timeZone)} to ${fullTime(to, timeZone)} (${zoneLabel(to, timeZone)})`);
    for (const note of notes) lines.push(`# ${note}`);
    lines.push(header.join(','), ...rows);
    return { csv: lines.join('\n') + '\n', from, to, rows: rows.length, series: cols.length };
}
