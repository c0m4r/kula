import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

async function importSource(relativePath) {
    const source = await readFile(new URL(relativePath, import.meta.url), 'utf8');
    const encoded = Buffer.from(source).toString('base64');
    return import(`data:text/javascript;base64,${encoded}`);
}

const {
    HistoryRequestController,
    liveHistoryRefreshInterval,
    updateLiveSampleInterval,
} = await importSource('../static/js/app/history-request.js');
const {
    insertHistoryGaps,
    annotateHistoryItems,
    historyItemContext,
    historySectionsForFocus,
    normalizeHistoryItem,
    normalizeValidAggregations,
    historyItemSample,
    historyItemTimestamp,
    resolveAggregation,
} = await importSource('../static/js/app/history-data.js');
const {
    formatChartTick,
    formatDateTimeInput,
    formatFullTimestamp,
    historyTooltipLines,
    normalizeTimeZone,
    parseDateTimeInput,
} = await importSource('../static/js/app/format.js');
const {
    ChartUpdateController,
    chartUpdates,
    setSharedCrosshair,
} = await importSource('../static/js/app/chart-controller.js');
const { attachCrosshairEvents, CrosshairState, panTimeRange, zoomTimeRange } =
    await importSource('../static/js/app/chart-interactions.js');
const {
    ViewportHistory,
    clampHistoryInterval,
    MIN_ZOOM_POINTS,
    minimumZoomSpan,
    fitZoomToObservations,
    zoomOutInterval,
} = await importSource('../static/js/app/history-navigation.js');
const {
    appendEnvelopeGap,
    appendEnvelopePoint,
    clearEnvelopeData,
    ensureSensorDatasets,
    envelopePlugin,
    envelopeRuns,
    hasEnvelopeData,
    measurementGapRects,
    nullAlignedData,
    trimEnvelopeData,
} = await importSource('../static/js/app/chart-envelope.js');
const {
    chartCSV,
    chartCursorText,
    chartDataSummary,
    chartSummaryText,
    chartTableModel,
    chartTimestamps,
    keyboardCursorTimestamp,
} = await importSource('../static/js/app/chart-accessibility.js');

function deferred() {
    let resolve;
    const promise = new Promise(res => { resolve = res; });
    return { promise, resolve };
}

test('zoom stops at twelve source intervals independently of output density', () => {
    assert.equal(MIN_ZOOM_POINTS, 12);
    for (const [resolution, interval, expected] of [
        ['1s', 1000, 12000], ['5s', 5000, 60000],
        ['1m', 1000, 720000], ['5m', 1000, 3600000],
        ['500ms', 500, 6000], ['invalid', 5000, 60000],
    ]) {
        const minimum = minimumZoomSpan(resolution, interval);
        assert.equal(minimum, expected);
        const bounded = clampHistoryInterval(999000, 1000000, 1000000, undefined, minimum);
        assert.equal(bounded.max - bounded.min, expected);
        assert.ok(bounded.max <= 1000000, 'minimum zoom must not extend into the future');
        const wider = clampHistoryInterval(0, 9000000, 10000000, undefined, minimum);
        assert.equal(wider.max - wider.min, 9000000, 'zoom out remains available');
    }
});

test('zoom counts observations rather than outages or duplicate timestamps', () => {
    const timestamps = [0, 1000, 2000, 3000, 4000, 5000, 60000, 61000, 62000, 63000, 64000, 65000];
    const range = fitZoomToObservations({ min: 60000, max: 65000 }, timestamps);
    assert.deepEqual(range, { min: 0, max: 65000 });
    assert.equal(fitZoomToObservations({ min: 0, max: 65000 }, timestamps.slice(1)), null);
    assert.equal(fitZoomToObservations({ min: 0, max: 65000 }, [NaN, ...Array(12).fill(1000)]), null);
    const dense = Array.from({ length: 100 }, (_, i) => i * 1000);
    const expanded = fitZoomToObservations({ min: 45000, max: 46000 }, dense);
    assert.equal(dense.filter(ts => ts >= expanded.min && ts <= expanded.max).length, 12);
});

function jsonResponse(payload, { ok = true, status = 200 } = {}) {
    return { ok, status, json: async () => payload };
}

test('only the latest history response is applied even when abort is ignored', async () => {
    const pending = [];
    const fetchImpl = (url, options) => {
        const request = deferred();
        pending.push({ url, options, ...request });
        return request.promise;
    };
    const controller = new HistoryRequestController(fetchImpl);
    const applied = [];
    const finished = [];

    const oldRequest = controller.fetchJSON('/old', {
        onApply: payload => applied.push(payload.id),
        onFinish: generation => finished.push(generation),
    });
    const newRequest = controller.fetchJSON('/new', {
        onApply: payload => applied.push(payload.id),
        onFinish: generation => finished.push(generation),
    });

    assert.equal(pending[0].options.signal.aborted, true);
    assert.equal(pending[1].options.signal.aborted, false);

    pending[1].resolve(jsonResponse({ id: 'new' }));
    assert.deepEqual(await newRequest, { status: 'applied', generation: 2 });

    // Resolve the old request after the current one. This fake transport does
    // not reject on abort, which exercises the generation guard itself.
    pending[0].resolve(jsonResponse({ id: 'old' }));
    assert.deepEqual(await oldRequest, { status: 'superseded', generation: 1 });
    assert.deepEqual(applied, ['new']);
    assert.deepEqual(finished, [2]);
});

test('HTTP failures never apply a history payload', async () => {
    const controller = new HistoryRequestController(async () =>
        jsonResponse({ samples: [{ ts: 'ignored' }] }, { ok: false, status: 503 }));
    let applied = false;
    let failure = null;
    let finished = false;

    const result = await controller.fetchJSON('/failure', {
        onApply: () => { applied = true; },
        onFailure: error => { failure = error; },
        onFinish: () => { finished = true; },
    });

    assert.equal(result.status, 'failed');
    assert.equal(applied, false);
    assert.match(failure.message, /503/);
    assert.equal(finished, true);
});

test('a locally served viewport advances the generation and aborts pending work', async () => {
    const request = deferred();
    let signal;
    const controller = new HistoryRequestController((url, options) => {
        signal = options.signal;
        return request.promise;
    });
    let applied = false;

    const pending = controller.fetchJSON('/remote', {
        onApply: () => { applied = true; },
    });
    assert.equal(controller.supersede(), 2);
    assert.equal(signal.aborted, true);

    request.resolve(jsonResponse({ id: 'stale' }));
    assert.equal((await pending).status, 'superseded');
    assert.equal(applied, false);
});

test('canonical history items preserve envelopes through buffering and redraw access', () => {
    const raw = { ts: '2026-09-02T10:00:00Z', cpu: { total: { usage: 40 } } };
    const wrappedRaw = normalizeHistoryItem(raw);
    assert.equal(wrappedRaw.data, raw);
    assert.equal(historyItemSample(wrappedRaw), raw);
    assert.equal(historyItemTimestamp(wrappedRaw), raw.ts);

    const envelope = {
        ts: '2026-09-02T10:01:00Z',
        dur: 60_000_000_000,
        data: { ts: '2026-09-02T10:01:00Z', value: 40 },
        min: { ts: '2026-09-02T10:01:00Z', value: 10 },
        max: { ts: '2026-09-02T10:01:00Z', value: 90 },
        sample_count: 60,
        source_tier: 1,
    };
    const buffered = [normalizeHistoryItem(envelope)];
    assert.equal(buffered[0], envelope);
    assert.equal(buffered[0].min.value, 10);
    assert.equal(buffered[0].max.value, 90);
    assert.equal(historyItemSample(buffered[0]).value, 40);

    const missingOuterTimestamp = normalizeHistoryItem({
        data: raw,
        min: { value: 1 },
        max: { value: 99 },
    });
    assert.equal(missingOuterTimestamp.ts, raw.ts);
    assert.equal(missingOuterTimestamp.min.value, 1);
    assert.equal(missingOuterTimestamp.max.value, 99);

    const gap = { _gap: true, ts: '2026-09-02T10:00:30Z' };
    assert.equal(normalizeHistoryItem(gap), gap);
    assert.equal(historyItemSample(gap), null);
});

test('history items retain compact per-response bucket provenance across redraws', () => {
    const items = annotateHistoryItems([
        {
            ts: '2026-09-02T10:01:00Z',
            dur: 60_000_000_000,
            bucket_start: '2026-09-02T10:00:00Z',
            bucket_end: '2026-09-02T10:01:00Z',
            sample_count: 60,
            coverage: 0.95,
            data: { value: 40 },
        },
        {
            ts: '2026-09-02T10:02:00Z',
            dur: 60_000_000_000,
            data: { value: 41 },
        },
    ], {
        tier: 1,
        resolution: '1m',
        source_resolution: '1s',
        complete: false,
        exact_complete: false,
        downsampled: true,
        valid_aggregations: ['data', 'min', 'max'],
    });

    const first = historyItemContext(items[0]);
    const second = historyItemContext(items[1]);
    assert.equal(first.bucketStart, Date.parse('2026-09-02T10:00:00Z'));
    assert.equal(first.bucketEnd, Date.parse('2026-09-02T10:01:00Z'));
    assert.equal(first.sampleCount, 60);
    assert.equal(first.coverage, 0.95);
    assert.equal(first.source, second.source, 'one source object is shared by the response');
    assert.equal(first.source.tier, 1);
    assert.equal(first.source.sourceResolution, '1s');
    assert.equal(first.source.complete, false);
    assert.equal(first.source.retentionComplete, false);
    assert.equal(first.source.downsampled, true);
    assert.equal(second.bucketStart, Date.parse('2026-09-02T10:01:00Z'),
        'legacy responses derive observed bounds from ts/dur');
    assert.equal(JSON.stringify(items[0]).includes('kulaHistoryContext'), false,
        'client-only context must not change canonical JSON');
});

test('timezone helpers keep UTC inputs exact and produce explicit history context', () => {
    const fractional = new Date('2026-09-02T10:04:05.123Z');
    assert.equal(formatDateTimeInput(fractional, 'utc', true), '2026-09-02T10:04:05');
    assert.equal(formatDateTimeInput(fractional, 'utc', true, true), '2026-09-02T10:04:05.123');
    const instant = new Date('2026-09-02T10:04:05Z');
    assert.equal(normalizeTimeZone('utc'), 'utc');
    assert.equal(normalizeTimeZone('Europe/Warsaw'), 'local');
    assert.equal(formatDateTimeInput(instant, 'utc'), '2026-09-02T10:04');
    assert.equal(parseDateTimeInput('2026-09-02T10:04', 'utc').toISOString(),
        '2026-09-02T10:04:00.000Z');
    assert.match(formatFullTimestamp(instant, 'utc', 'en-GB'), /2026/);
    assert.match(formatFullTimestamp(instant, 'utc', 'en-GB'), /UTC/);
    assert.match(formatChartTick(instant, 'utc', 'second', 'en-GB'), /10:04:05/);

    const lines = historyTooltipLines({
        bucketStart: Date.parse('2026-09-02T10:03:00Z'),
        bucketEnd: instant.getTime(),
        sampleCount: 42,
        coverage: 0.875,
        source: {
            tier: 1,
            resolution: '1m',
            sourceResolution: '1s',
            complete: false,
            validAggregations: ['data', 'min', 'max'],
        },
    }, { mode: 'utc', locale: 'en-GB', aggregation: 'avg' });
    assert.ok(lines.some(line => line.startsWith('bucket_start:') && line.includes('UTC')));
    assert.ok(lines.some(line => line.startsWith('bucket_end:') && line.includes('UTC')));
    assert.ok(lines.includes('source: tier 2 · 1s'));
    assert.ok(lines.includes('output_resolution: 1m'));
    assert.ok(lines.includes('contributors: 42 source_records'));
    assert.ok(lines.includes('coverage: 87.5% · partial_range'));
    assert.ok(lines.includes('representative: policy_center'));
    assert.ok(lines.includes('range_band: bucket_minimum–bucket_maximum'));
});

test('chart envelope storage stays aligned through raw points, gaps, trimming, and clearing', () => {
    const dataset = { data: [] };
    appendEnvelopePoint(dataset, new Date(1000), 5, null, null);
    assert.equal(dataset.$kulaEnvelope, undefined, 'raw points do not allocate extrema storage');

    appendEnvelopePoint(dataset, new Date(2000), 6, 3, 9);
    assert.deepEqual(dataset.$kulaEnvelope, [null, null, 3, 9]);
    appendEnvelopeGap(dataset, new Date(3000));
    appendEnvelopePoint(dataset, new Date(4000), 7, 10, 4);
    appendEnvelopePoint(dataset, new Date(5000), 8, 5, 11, { detail: 'kept' });
    assert.deepEqual(dataset.$kulaEnvelope, [
        null, null,
        3, 9,
        null, null,
        4, 10,
        5, 11,
    ]);
    assert.equal(dataset.data[4].detail, 'kept');
    assert.deepEqual(envelopeRuns(dataset), [[3, 4]], 'a null gap splits drawable bands');

    trimEnvelopeData(dataset, 2);
    assert.equal(dataset.data.length, 3);
    assert.deepEqual(dataset.$kulaEnvelope, [null, null, 4, 10, 5, 11]);
    clearEnvelopeData(dataset);
    assert.deepEqual(dataset.data, []);
    assert.equal(dataset.$kulaEnvelope, undefined);
});

test('new dynamic series are null-aligned to retained peer timestamps', () => {
    const first = { data: [{ x: 1000, y: 10 }, { x: 2000, y: 20 }] };
    assert.deepEqual(nullAlignedData([first], 3000), [
        { x: 1000, y: null },
        { x: 2000, y: null },
    ]);
    assert.deepEqual(nullAlignedData([], 3000), []);
});

test('sensor identities and retained values survive reorder, gaps, and trimming', () => {
    const chart = { data: { datasets: [] } };
    const palette = [['blue', 'lightblue'], ['red', 'pink']];
    const [original] = ensureSensorDatasets(chart, ['Package'], palette, 1000);
    appendEnvelopePoint(original, 1000, 0, 0, 1);
    assert.equal(hasEnvelopeData(original), true, 'zero is a real observation');
    ensureSensorDatasets(chart, ['Core', 'Package'], palette, 2000);
    assert.equal(chart.data.datasets[0], original);
    const added = chart.data.datasets[1];
    assert.deepEqual(added.data, [{ x: 1000, y: null }]);
    appendEnvelopeGap(original, 2000);
    appendEnvelopePoint(added, 2000, 42, 40, 44);
    ensureSensorDatasets(chart, [], palette, 3000);
    assert.equal(chart.data.datasets.length, 2, 'absent sensors retain their history');
    trimEnvelopeData(original, 1);
    assert.equal(hasEnvelopeData(original), false);
    ensureSensorDatasets(chart, [], palette, 3000);
    assert.deepEqual(chart.data.datasets, [added], 'fully expired sensors are pruned');
    clearEnvelopeData(added);
    assert.equal(hasEnvelopeData(added), false);
});

test('envelope plugin expands automatic axes and paints bands without extra datasets', () => {
    const dataset = { borderColor: '#3b82f6', data: [] };
    appendEnvelopePoint(dataset, 1000, 4, 1, 7);
    appendEnvelopePoint(dataset, 2000, 6, 2, 9);
    const calls = [];
    const ctx = {
        save() { calls.push('save'); },
        restore() { calls.push('restore'); },
        beginPath() { calls.push('begin'); },
        rect() { calls.push('rect'); },
        clip() { calls.push('clip'); },
        moveTo() { calls.push('move'); },
        lineTo() { calls.push('line'); },
        closePath() { calls.push('close'); },
        fill() { calls.push('fill'); },
        set fillStyle(value) { calls.push(`color:${value}`); },
    };
    const chart = {
        ctx,
        chartArea: { left: 0, right: 100, top: 0, bottom: 50 },
        data: { datasets: [dataset] },
        scales: {
            x: { getPixelForValue: value => value / 100 },
            y: { getPixelForValue: value => 50 - value },
        },
        isDatasetVisible: () => true,
    };

    const scale = { axis: 'y', id: 'y', options: {}, min: 4, max: 6 };
    envelopePlugin.afterDataLimits(chart, { scale });
    assert.deepEqual({ min: scale.min, max: scale.max }, { min: 1, max: 9 });
    envelopePlugin.beforeDatasetsDraw(chart, {}, {});
    assert.equal(calls.filter(call => call === 'fill').length, 1);
    assert.equal(chart.data.datasets.length, 1, 'bands do not triple the Chart.js dataset count');

    const fixedScale = { axis: 'y', id: 'y', options: { min: 0, max: 8 }, min: 0, max: 8 };
    envelopePlugin.afterDataLimits(chart, { scale: fixedScale });
    assert.deepEqual({ min: fixedScale.min, max: fixedScale.max }, { min: 0, max: 8 });
});



test('chart accessibility exposes bounded tables and complete formula-safe CSV', () => {
    const first = {
        label: '=CPU',
        yAxisID: 'y',
        data: [
            { x: 1000, y: 10 },
            { x: 2000, y: 20 },
            { x: 3000, y: 30 },
        ],
        $kulaEnvelope: [5, 15, 10, 25, 20, 40],
    };
    const second = {
        label: 'System',
        data: [{ x: 1000, y: 2 }, { x: 3000, y: 4 }],
    };
    const chart = {
        data: { datasets: [first, second] },
        scales: {
            x: { min: 1000, max: 3000 },
            y: { options: { ticks: { callback: value => `${value}%` } } },
        },
        isDatasetVisible: () => true,
        $kulaHistoryStatus: 'partial',
        $kulaAccessibility: { title: { textContent: 'CPU Usage' } },
    };
    const options = {
        translate: key => key,
        formatTimestamp: value => `t${value}`,
    };

    assert.deepEqual(chartTimestamps(chart), [1000, 2000, 3000]);
    assert.deepEqual(chartDataSummary(chart), {
        series: 2,
        points: 3,
        first: 1000,
        last: 3000,
        empty: false,
    });
    assert.match(chartSummaryText(chart, options), /history partial/);

    const preview = chartTableModel(chart, { ...options, limit: 2 });
    assert.equal(preview.totalRows, 3);
    assert.equal(preview.rows.length, 2);
    assert.equal(preview.omittedRows, 1);
    assert.equal(preview.columns.length, 4, 'value, min, max, and second series');

    const csv = chartCSV(chart, options);
    assert.equal(csv.split('\r\n').filter(Boolean).length, 4, 'CSV contains every timestamp');
    assert.match(csv, /"'=CPU"/, 'spreadsheet formulas are neutralized');
});

test('keyboard chart exploration snaps to plotted observations and announces values', () => {
    const chart = {
        data: {
            datasets: [{
                label: 'Usage',
                data: [
                    { x: 1000, y: 1 },
                    { x: 2000, y: 2 },
                    { x: 4000, y: 4 },
                ],
            }],
        },
        scales: {
            x: { min: 1000, max: 4000 },
            y: { options: { ticks: { callback: value => `${value}%` } } },
        },
        isDatasetVisible: () => true,
        $kulaAccessibility: { title: { textContent: 'CPU Usage' } },
    };

    assert.equal(keyboardCursorTimestamp(chart, 'Enter'), 2000);
    assert.equal(keyboardCursorTimestamp(chart, 'ArrowLeft', 2000), 1000);
    assert.equal(keyboardCursorTimestamp(chart, 'ArrowRight', 2000), 4000);
    assert.equal(keyboardCursorTimestamp(chart, 'Home', 4000), 1000);
    assert.equal(keyboardCursorTimestamp(chart, 'End', 1000), 4000);
    const announcement = chartCursorText(chart, 2000, {
        translate: key => key,
        formatTimestamp: value => `t${value}`,
    });
    assert.match(announcement, /CPU Usage\. t2000/);
    assert.match(announcement, /Usage: 2%/);
});

test('history gaps retain exact bounds for neutral shaded regions', () => {
    const base = Date.parse('2026-09-03T10:00:00Z');
    const items = [0, 1, 2, 6].map(seconds => ({
        ts: new Date(base + seconds * 1000).toISOString(),
        data: { ts: new Date(base + seconds * 1000).toISOString() },
    }));
    const withGaps = insertHistoryGaps(items, '1s');
    assert.equal(withGaps.length, 5);
    assert.equal(withGaps[3]._gap, true);
    assert.equal(Date.parse(withGaps[3].gap_start), base + 3000);
    assert.equal(Date.parse(withGaps[3].gap_end), base + 6000);
    assert.deepEqual(measurementGapRects([
        { start: 5, end: 25 },
        { start: -10, end: 5 },
        { start: 8, end: 8 },
    ], { getPixelForValue: value => value * 2 }, { left: 0, right: 40 }), [
        { left: 10, right: 40 },
        { left: 0, right: 10 },
    ]);
});



test('unsupported aggregation operations fall back to representative data', () => {
    assert.deepEqual(normalizeValidAggregations(undefined), ['data']);
    assert.deepEqual(normalizeValidAggregations(['max']), ['data']);
    assert.deepEqual(normalizeValidAggregations(['data', 'min', 'min', 'bogus']), ['data', 'min']);

    assert.deepEqual(resolveAggregation('max', ['data']), {
        selection: 'avg',
        field: 'data',
        valid: ['data'],
    });
    assert.deepEqual(resolveAggregation('max', ['data', 'max']), {
        selection: 'max',
        field: 'max',
        valid: ['data', 'max'],
    });
});

test('chart updates are animation-frame batched and off-screen work is deferred', () => {
    const frames = [];
    let observeCallback;
    const controller = new ChartUpdateController({
        scheduleFrame: callback => { frames.push(callback); return frames.length; },
        cancelFrame: () => {},
        observerFactory: callback => {
            observeCallback = callback;
            return { observe() {}, unobserve() {} };
        },
    });

    globalThis.window = { innerWidth: 1000, innerHeight: 800 };
    const target = top => ({
        classList: { contains: () => false },
        getBoundingClientRect: () => ({ top, bottom: top + 300, left: 0, right: 600, width: 600, height: 300 }),
    });
    const visibleTarget = target(10);
    let hiddenTop = 1200;
    const hiddenTarget = {
        classList: { contains: () => false },
        getBoundingClientRect: () => ({ top: hiddenTop, bottom: hiddenTop + 300, left: 0, right: 600, width: 600, height: 300 }),
    };
    const makeChart = element => ({
        canvas: { closest: () => element },
        updates: 0,
        update() { this.updates++; },
        destroy() {},
    });
    const visible = makeChart(visibleTarget);
    const hidden = makeChart(hiddenTarget);
    controller.register(visible);
    controller.register(hidden);

    controller.markAll();
    controller.markAll();
    assert.equal(frames.length, 1, 'multiple invalidations share one frame');
    frames.shift()();
    assert.equal(visible.updates, 1);
    assert.equal(hidden.updates, 0);

    hiddenTop = 100;
    observeCallback([{ target: hiddenTarget, isIntersecting: true }]);
    assert.equal(frames.length, 1);
    frames.shift()();
    assert.equal(hidden.updates, 1, 'dirty chart catches up when it enters the viewport');
    delete globalThis.window;
});

test('chart visibility observer uses the same strict viewport boundary as rendering', () => {
    let options;
    globalThis.IntersectionObserver = class {
        constructor(_callback, observerOptions) { options = observerOptions; }
        observe() {}
        unobserve() {}
    };
    const controller = new ChartUpdateController();
    assert.equal(options.rootMargin, '0px');
    controller.clear();
    delete globalThis.IntersectionObserver;
});

test('render-only chart work stays cheap and visibility reads precede canvas writes', () => {
    const frames = [];
    const events = [];
    globalThis.window = { innerWidth: 1000, innerHeight: 800 };
    const makeChart = name => {
        const target = {
            classList: { contains: () => false },
            getBoundingClientRect() {
                events.push(`read:${name}`);
                return { top: 0, bottom: 300, left: 0, right: 600, width: 600, height: 300 };
            },
        };
        return {
            canvas: { closest: () => target },
            updates: 0,
            renders: 0,
            update() { events.push(`update:${name}`); this.updates++; },
            render() { events.push(`render:${name}`); this.renders++; },
            destroy() {},
        };
    };
    const controller = new ChartUpdateController({
        scheduleFrame: callback => { frames.push(callback); return frames.length; },
    });
    const first = makeChart('first');
    const second = makeChart('second');
    controller.register(first);
    controller.register(second);
    events.length = 0;

    controller.mark(first, 'render');
    controller.mark(second, 'render');
    controller.mark(first, 'update'); // data work must subsume cursor-only work
    frames.shift()();

    assert.deepEqual(events.slice(0, 2), ['read:first', 'read:second']);
    assert.equal(first.updates, 1);
    assert.equal(first.renders, 0);
    assert.equal(second.updates, 0);
    assert.equal(second.renders, 1);
    controller.clear();
    delete globalThis.window;
});

test('pan and pinch range math preserves direction, anchor, and duration', () => {
    assert.deepEqual(panTimeRange(0, 1000, 100, 500), { min: -200, max: 800 });
    assert.deepEqual(zoomTimeRange(0, 1000, 0.5, 0.5), { min: 250, max: 750 });
    assert.deepEqual(zoomTimeRange(0, 1000, 0.5, 0), { min: 0, max: 500 });
});

test('viewport history has deterministic back/forward and truncates forward branches', () => {
    const history = new ViewportHistory({ kind: 'preset', range: 300 });
    history.push({ kind: 'custom', from: new Date(1000), to: new Date(2000) });
    history.push({ kind: 'custom', from: new Date(1200), to: new Date(1800) });

    assert.equal(history.back().from.getTime(), 1000);
    assert.equal(history.back().range, 300);
    assert.equal(history.canBack(), false);
    assert.equal(history.forward().from.getTime(), 1000);

    history.push({ kind: 'preset', range: 900 });
    assert.equal(history.canForward(), false);
    assert.equal(history.current().range, 900);
});

test('zoom-out doubles an exact interval without extending beyond now', () => {
    const expanded = zoomOutInterval(new Date(8000), new Date(10000), 2, new Date(10000));
    assert.equal(expanded.from.getTime(), 6000);
    assert.equal(expanded.to.getTime(), 10000);
});

test('all gesture ranges are clamped to the server history contract', () => {
    const day = 86400000;
    assert.deepEqual(clampHistoryInterval(0, 40 * day, 50 * day), {
        min: 4.5 * day,
        max: 35.5 * day,
    });
    assert.deepEqual(clampHistoryInterval(45 * day, 55 * day, 50 * day), {
        min: 40 * day,
        max: 50 * day,
    });
});

test('live interval estimation learns slow collectors without one jitter becoming permanent', () => {
    let observed = { intervals: [], estimate: null };
    for (const delta of [5000, 5100, 4900, 1000, 5000]) {
        observed = updateLiveSampleInterval(observed.intervals, delta);
    }
    assert.equal(observed.estimate, 5000);
    assert.equal(liveHistoryRefreshInterval(3600, 1000, observed.estimate), 0);
    assert.equal(liveHistoryRefreshInterval(3600, 1000, 1000), 3604);
});

test('shared crosshair hover clears while a pinned timestamp persists', () => {
    const crosshair = new CrosshairState();
    assert.deepEqual(crosshair.apply('hover', 100), { timestamp: 100, pinned: false });
    assert.deepEqual(crosshair.apply('pin', 100), { timestamp: 100, pinned: true });
    assert.deepEqual(crosshair.apply('clear'), { timestamp: 100, pinned: true });
    assert.deepEqual(crosshair.apply('move', 200), { timestamp: 200, pinned: true });
    assert.deepEqual(crosshair.apply('unpin'), { timestamp: null, pinned: false });
});

test('a drag gesture does not become a crosshair pin click', () => {
    const listeners = new Map();
    const canvas = {
        addEventListener(name, listener) { listeners.set(name, listener); },
        removeEventListener(name) { listeners.delete(name); },
        getBoundingClientRect() { return { left: 0 }; },
    };
    const chart = {
        canvas,
        chartArea: { left: 0, right: 100 },
        scales: { x: { getValueForPixel: value => value } },
    };
    const events = [];
    const previousDocument = globalThis.document;
    globalThis.document = { dispatchEvent: event => events.push(event.detail) };
    const cleanup = attachCrosshairEvents(chart);
    try {
        listeners.get('pointerdown')({ button: 0, isPrimary: true, pointerId: 1, clientX: 10, clientY: 10 });
        listeners.get('pointermove')({ pointerId: 1, clientX: 30, clientY: 10 });
        listeners.get('pointerup')({ pointerId: 1 });
        listeners.get('click')({ clientX: 30 });
        assert.equal(events.some(event => event.action === 'pin'), false);

        listeners.get('click')({ clientX: 40 });
        assert.equal(events.at(-1).action, 'pin');
        assert.equal(events.at(-1).timestamp, 40);
    } finally {
        cleanup();
        if (previousDocument === undefined) delete globalThis.document;
        else globalThis.document = previousDocument;
    }
});

test('shared crosshair invalidation renders without updating chart scales', () => {
    const chart = {
        canvas: { closest: () => null },
        renders: 0,
        updates: 0,
        render() { this.renders++; },
        update() { this.updates++; },
        destroy() {},
    };
    chartUpdates.register(chart);
    setSharedCrosshair(1234);
    chartUpdates.flush();
    assert.equal(chart.$kulaCrosshairTimestamp, 1234);
    assert.equal(chart.renders, 1);
    assert.equal(chart.updates, 0);
    chartUpdates.clear();
});

test('history sections stay complete normally and narrow in Focus Mode', () => {
    assert.equal(historySectionsForFocus(false, ['card-cpu']), null);
    assert.deepEqual(historySectionsForFocus(true, ['card-disk-io', 'card-pg-tps']), [
        'apps', 'cpu', 'disk', 'lavg', 'mem', 'net', 'swap', 'sys',
    ]);

    for (const cardId of [
        'card-pg-connections',
        'card-mysql-connections',
        'card-nginx-connections',
        'card-containers-disk-r',
        'card-containers-disk-w',
    ]) {
        const sections = historySectionsForFocus(true, [cardId]);
        assert.ok(sections.includes('apps'), `${cardId} must request apps history`);
        if (cardId.includes('containers-disk')) {
            assert.equal(sections.includes('disk'), false, `${cardId} is not a host disk card`);
        }
    }
});







test('rolling history refreshes at display resolution while short views stream', () => {
    assert.equal(liveHistoryRefreshInterval(300, 500, 1000), 0);
    assert.ok(liveHistoryRefreshInterval(86400, 720, 1000) >= 120000);
    assert.ok(liveHistoryRefreshInterval(2592000, 5000, 1000) > 500000);
    assert.equal(liveHistoryRefreshInterval(300, 500, 100), 1000);
});

test('chart labels reuse bounded formatters across every chart', () => {
    const NativeFormatter = Intl.DateTimeFormat;
    let constructions = 0;
    Intl.DateTimeFormat = new Proxy(NativeFormatter, {
        construct(target, args) { constructions++; return new target(...args); },
    });
    try {
        for (let i = 0; i < 30000; i++) {
            formatChartTick(1720000000000 + i * 1000, 'utc', 'second', 'en-GB');
        }
        assert.ok(constructions <= 1, `constructed ${constructions} formatters`);
        formatChartTick(1720000000000, 'local', 'second', 'en-GB');
        assert.ok(constructions <= 2, 'timezone has its own formatter');
    } finally { Intl.DateTimeFormat = NativeFormatter; }
});
