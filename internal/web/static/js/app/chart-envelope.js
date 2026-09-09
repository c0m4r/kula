/* ============================================================
   chart-envelope.js — Compact per-series Min/Max storage and a
   Chart.js plugin that renders trustworthy historical bands.
   ============================================================ */
'use strict';

const ENVELOPE_KEY = '$kulaEnvelope';

function finiteNumber(value) {
    return typeof value === 'number' && Number.isFinite(value);
}

function pointTime(point) {
    if (!point || point.x == null) return NaN;
    const value = point.x instanceof Date ? point.x.getTime() : Number(point.x);
    return Number.isFinite(value) ? value : NaN;
}

function envelopeRange(minimum, maximum) {
    if (!finiteNumber(minimum) || !finiteNumber(maximum)) return null;
    return minimum <= maximum ? [minimum, maximum] : [maximum, minimum];
}

/**
 * Append a chart point and, when supplied, its trustworthy extrema. Extrema
 * use one flat array (min,max,min,max,...) rather than two Chart.js datasets.
 */
export function appendEnvelopePoint(dataset, x, y, minimum, maximum, extra = null) {
    if (!dataset) return;
    // Chart.js parsing is disabled. Its time/linear scales require numeric
    // milliseconds and finite values, with explicit nulls for missing data.
    x = x instanceof Date ? x.getTime() : x;
    if (!finiteNumber(x)) return;
    y = finiteNumber(y) ? y : null;
    if (!Array.isArray(dataset.data)) dataset.data = [];

    const oldLength = dataset.data.length;
    const point = extra && typeof extra === 'object'
        ? { x, y, ...extra }
        : { x, y };
    hasEnvelopeData(dataset);
    dataset.data.push(point);
    if (point.y != null) dataset.$kulaValueCount++;

    const range = envelopeRange(minimum, maximum);
    if (!Array.isArray(dataset[ENVELOPE_KEY]) && range) {
        dataset[ENVELOPE_KEY] = new Array(oldLength * 2).fill(null);
    }
    if (Array.isArray(dataset[ENVELOPE_KEY])) {
        dataset[ENVELOPE_KEY].push(range?.[0] ?? null, range?.[1] ?? null);
    }
}

/** Append a null point while preserving companion-array alignment. */
export function appendEnvelopeGap(dataset, x) {
    appendEnvelopePoint(dataset, x, null, null, null);
}

// Backfill a newly discovered dynamic series with null points using timestamps
// from an existing peer. This keeps stable-identity datasets aligned without
// inventing values before the sensor/application existed.
export function nullAlignedData(datasets, fallbackX) {
    if (!Array.isArray(datasets) || datasets.length === 0) return [];
    let peer = null;
    for (const dataset of datasets) {
        if (Array.isArray(dataset?.data) && (!peer || dataset.data.length > peer.data.length)) {
            peer = dataset;
        }
    }
    if (!peer || peer.data.length === 0) return [];
    return peer.data.map(point => ({
        x: Number(point && typeof point === 'object' && point.x != null ? point.x : fallbackX),
        y: null,
    }));
}

/** Track visible observations without rescanning the entire history per tick. */
export function hasEnvelopeData(dataset) {
    if (!dataset || !Array.isArray(dataset.data)) return false;
    if (dataset.$kulaValueCount == null) {
        dataset.$kulaValueCount = dataset.data.reduce((count, point) => count + (point?.y != null ? 1 : 0), 0);
    }
    return dataset.$kulaValueCount > 0;
}

/** Keep sensor identities stable when their order or availability changes. */
export function ensureSensorDatasets(chart, names, colorPairs, ts) {
    if (!chart?.data?.datasets) return [];
    const active = new Set(names);
    chart.data.datasets = chart.data.datasets.filter(dataset =>
        active.has(dataset.$kulaSensorName || dataset.label) || hasEnvelopeData(dataset));
    for (const name of names) {
        let dataset = chart.data.datasets.find(candidate =>
            (candidate.$kulaSensorName || candidate.label) === name);
        if (!dataset) {
            const pair = colorPairs[chart.data.datasets.length % colorPairs.length];
            dataset = {
                label: name,
                borderColor: pair[0],
                backgroundColor: pair[1],
                fill: false,
                tension: 0,
                data: nullAlignedData(chart.data.datasets, ts),
                $kulaValueCount: 0,
                pointHitRadius: 5,
            };
            chart.data.datasets.push(dataset);
        }
        dataset.$kulaSensorName = name;
    }
    return chart.data.datasets;
}

/** Remove data and extrema together. */
export function clearEnvelopeData(dataset) {
    if (!dataset) return;
    dataset.data = [];
    dataset.$kulaValueCount = 0;
    delete dataset[ENVELOPE_KEY];
}

/** Remove the same leading point count from data and the flat extrema array. */
export function trimEnvelopeData(dataset, count) {
    if (!dataset || !Array.isArray(dataset.data) || count <= 0) return;
    const removed = Math.min(count, dataset.data.length);
    if (dataset.$kulaValueCount != null) {
        for (let i = 0; i < removed; i++) {
            if (dataset.data[i]?.y != null) dataset.$kulaValueCount--;
        }
    }
    dataset.data.splice(0, removed);
    if (Array.isArray(dataset[ENVELOPE_KEY])) {
        dataset[ENVELOPE_KEY].splice(0, removed * 2);
    }
}

/** Return consecutive drawable index runs; exported for deterministic tests. */
export function envelopeRuns(dataset) {
    const data = dataset?.data;
    const envelope = dataset?.[ENVELOPE_KEY];
    if (!Array.isArray(data) || !Array.isArray(envelope)) return [];

    const runs = [];
    let start = -1;
    for (let i = 0; i < data.length; i++) {
        const valid = finiteNumber(data[i]?.y) && Number.isFinite(pointTime(data[i])) &&
            finiteNumber(envelope[i * 2]) && finiteNumber(envelope[i * 2 + 1]);
        if (valid && start < 0) start = i;
        if ((!valid || i === data.length - 1) && start >= 0) {
            const end = valid && i === data.length - 1 ? i : i - 1;
            if (end > start) runs.push([start, end]);
            start = -1;
        }
    }
    return runs;
}

function rgba(color, alpha = 0.13) {
    if (typeof color !== 'string') return `rgba(59, 130, 246, ${alpha})`;
    const hex = color.match(/^#([0-9a-f]{6})$/i);
    if (hex) {
        const raw = hex[1];
        return `rgba(${parseInt(raw.slice(0, 2), 16)}, ${parseInt(raw.slice(2, 4), 16)}, ${parseInt(raw.slice(4, 6), 16)}, ${alpha})`;
    }
    const rgb = color.match(/^rgba?\(\s*([\d.]+)\s*,\s*([\d.]+)\s*,\s*([\d.]+)/i);
    if (rgb) return `rgba(${rgb[1]}, ${rgb[2]}, ${rgb[3]}, ${alpha})`;
    return `rgba(59, 130, 246, ${alpha})`;
}

function datasetAxisID(dataset) {
    return dataset.yAxisID || 'y';
}

/** Convert missing-observation intervals into clipped chart-area rectangles. */
export function measurementGapRects(gaps, xScale, chartArea) {
    if (!Array.isArray(gaps) || !xScale?.getPixelForValue || !chartArea) return [];
    const rects = [];
    for (const gap of gaps) {
        const start = Number(gap?.start);
        const end = Number(gap?.end);
        if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start) continue;
        const first = Number(xScale.getPixelForValue(start));
        const second = Number(xScale.getPixelForValue(end));
        if (!Number.isFinite(first) || !Number.isFinite(second)) continue;
        const left = Math.max(chartArea.left, Math.min(first, second));
        const right = Math.min(chartArea.right, Math.max(first, second));
        if (right > left) rects.push({ left, right });
    }
    return rects;
}

function drawMeasurementGaps(chart, xScale, options) {
    const ranges = typeof options?.gaps === 'function' ? options.gaps(chart) : options?.gaps;
    const rects = measurementGapRects(ranges, xScale, chart.chartArea);
    if (rects.length === 0) return;
    const { top, bottom } = chart.chartArea;
    chart.ctx.fillStyle = options?.gapColor || 'rgba(148, 163, 184, 0.12)';
    rects.forEach(rect => chart.ctx.fillRect(rect.left, top, rect.right - rect.left, bottom - top));
}

function drawDatasetEnvelope(chart, dataset, xScale, yScale) {
    const envelope = dataset[ENVELOPE_KEY];
    const runs = envelopeRuns(dataset);
    if (runs.length === 0) return;

    const ctx = chart.ctx;
    ctx.fillStyle = dataset.$kulaEnvelopeColor || rgba(dataset.borderColor);
    for (const [start, end] of runs) {
        ctx.beginPath();
        for (let i = start; i <= end; i++) {
            const x = xScale.getPixelForValue(pointTime(dataset.data[i]));
            const y = yScale.getPixelForValue(envelope[i * 2 + 1]);
            if (i === start) ctx.moveTo(x, y);
            else ctx.lineTo(x, y);
        }
        for (let i = end; i >= start; i--) {
            ctx.lineTo(
                xScale.getPixelForValue(pointTime(dataset.data[i])),
                yScale.getPixelForValue(envelope[i * 2]),
            );
        }
        ctx.closePath();
        ctx.fill();
    }
}

export const envelopePlugin = {
    id: 'kulaEnvelope',

    // Include the envelope in automatic Y scaling. Explicit min/max settings
    // remain authoritative and intentionally clip out-of-bound observations.
    afterDataLimits(chart, { scale }) {
        if (scale?.axis !== 'y') return;
        let minimum = Infinity;
        let maximum = -Infinity;
        chart.data.datasets.forEach((dataset, index) => {
            if (!chart.isDatasetVisible(index) || datasetAxisID(dataset) !== scale.id) return;
            const envelope = dataset[ENVELOPE_KEY];
            if (!Array.isArray(envelope)) return;
            for (let i = 0; i < envelope.length; i += 2) {
                if (finiteNumber(envelope[i])) minimum = Math.min(minimum, envelope[i]);
                if (finiteNumber(envelope[i + 1])) maximum = Math.max(maximum, envelope[i + 1]);
            }
        });
        if (scale.options?.min == null && minimum !== Infinity) {
            scale.min = finiteNumber(scale.min) ? Math.min(scale.min, minimum) : minimum;
        }
        if (scale.options?.max == null && maximum !== -Infinity) {
            scale.max = finiteNumber(scale.max) ? Math.max(scale.max, maximum) : maximum;
        }
    },

    beforeDatasetsDraw(chart, _args, options) {
        if (!chart.chartArea) return;
        const xScale = chart.scales.x;
        if (!xScale) return;

        const { left, right, top, bottom } = chart.chartArea;
        chart.ctx.save();
        chart.ctx.beginPath();
        chart.ctx.rect(left, top, right - left, bottom - top);
        chart.ctx.clip();
        drawMeasurementGaps(chart, xScale, options);
        if (options?.enabled === false) {
            chart.ctx.restore();
            return;
        }
        const principal = chart.data.datasets.findIndex((dataset, index) =>
            chart.isDatasetVisible(index) && dataset.kulaPrincipal === true);
        let drawn = false;
        chart.data.datasets.forEach((dataset, index) => {
            if (!chart.isDatasetVisible(index)) return;
            if (!options?.allSeries && (principal >= 0 ? index !== principal : drawn)) return;
            const yScale = chart.scales[datasetAxisID(dataset)];
            if (yScale && envelopeRuns(dataset).length > 0) {
                drawDatasetEnvelope(chart, dataset, xScale, yScale);
                drawn = true;
            }
        });
        chart.ctx.restore();
    },
};
