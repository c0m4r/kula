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
    historyViewAcceptsLiveSamples,
    liveHistoryRefreshInterval,
    updateLiveSampleInterval,
} = await importSource('../static/js/app/history-request.js');
const { diskKey, diskMember, diskLabel, diskDOMKey, migrateDiskSelection } =
    await importSource('../static/js/app/disk-identity.js');

test('disk selection and envelopes follow identity through kernel renames', () => {
    const old = [{ id: 'wwid:A', name: 'sda', read_bps: 10 }, { id: 'wwid:B', name: 'sdb', read_bps: 100 }];
    const renamed = [{ id: 'wwid:B', name: 'sda', read_bps: 200 }, { id: 'wwid:A', name: 'sdb', read_bps: 20 }];
    const key = migrateDiskSelection('sda', old);
    assert.equal(key, 'wwid:A');
    assert.equal(diskMember(renamed, key).read_bps, 20);
    assert.equal(diskMember(old, diskKey(renamed[1])).read_bps, 10);
    assert.equal(diskMember([{ name: 'sda', read_bps: 999 }], key), undefined);
    assert.equal(migrateDiskSelection(key, renamed), key);
    assert.equal(migrateDiskSelection(key, [{ id: 'wwid:C', name: 'sda' }]), key);
    assert.equal(migrateDiskSelection(key, []), key);
    assert.equal(diskKey({ name: 'sda' }), 'kernel:sda');
    assert.equal(migrateDiskSelection('kernel:sda', old), 'kernel:sda');
    assert.match(diskLabel({ name: 'sda' }), /unstable/);
    assert.notEqual(diskDOMKey('serial:a|b'), diskDOMKey('serial:a_b'));
});
const {
    insertHistoryGaps,
    annotateHistoryItems,
    historyItemContext,
    historyItemExtrema,
    historySectionsForFocus,
    normalizeHistoryItem,
    normalizeValidAggregations,
    historyItemSample,
    historyItemTimestamp,
    resolveAggregation,
} = await importSource('../static/js/app/history-data.js');
const {
    clockFace,
    existingDateTimeInput,
    formatChartTick,
    formatDateTimeInput,
    formatFullTimestamp,
    formatMetricNumber,
    formatTimeOfDay,
    historyTooltipLines,
    normalizeTimeZone,
    parseDateTimeInput,
    parseTimeOfDay,
    stepTimeOfDay,
    timeOfDayParts,
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

test('legacy extrema are selected per field and retain provenance through export and trimming', () => {
    const legacy = { ts: new Date(1000).toISOString(), extrema_profile: 'legacy',
        data: { cpu: { total: { usage: 5 } }, net: { tcp: { curr_estab: 3 } } },
        min: { cpu: { total: { usage: 1 } }, net: { tcp: { curr_estab: 2 } } },
        max: { cpu: { total: { usage: 9 } }, net: { tcp: { curr_estab: 2 } } } };
    const current = structuredClone(legacy);
    current.ts = new Date(2000).toISOString();
    current.extrema_profile = 'current';
    current.max.net.tcp.curr_estab = 4;
    const response = { valid_aggregations: ['data'], available_aggregations: ['data', 'min', 'max'],
        extrema_profiles: { legacy: ['cpu.total.usage'], current: ['*'], none: [] } };
    const items = annotateHistoryItems([legacy, current], response);
    const cpu = { label: 'CPU', data: [] }, connections = { label: 'Connections', data: [] };
    for (const item of items) {
        const { minimum, maximum, profile } = historyItemExtrema(item, ['data', 'min', 'max']);
        appendEnvelopePoint(cpu, Date.parse(item.ts), item.data.cpu.total.usage,
            minimum?.cpu?.total?.usage, maximum?.cpu?.total?.usage, null, 'max', profile);
        appendEnvelopePoint(connections, Date.parse(item.ts), item.data.net.tcp.curr_estab,
            minimum?.net?.tcp?.curr_estab, maximum?.net?.tcp?.curr_estab, null, 'max', profile);
    }
    assert.deepEqual(cpu.data.map(point => point.y), [9, 9]);
    assert.deepEqual(connections.data.map(point => point.y), [null, 4]);
    assert.equal(connections.data[0].extremaUnavailable, true);
    assert.equal(connections.data[0].extremaSource, 'unavailable');
    assert.equal(cpu.data[0].extremaSource, 'legacy');
    assert.deepEqual(connections.$kulaEnvelope, [null, null, 2, 4]);
    assert.equal(legacy.max.net.tcp.curr_estab, 2, 'projection must not modify retained observations');
    const chart = { data: { datasets: [cpu, connections] } };
    const csv = chartCSV(chart);
    assert.match(csv, /CPU — extrema source/);
    assert.match(csv, /legacy/);
    assert.match(csv, /unavailable/);
    const lines = historyTooltipLines(historyItemContext(items[0]), {
        aggregation: 'max', extremaSources: ['legacy', 'unavailable'],
    });
    assert(lines.includes('history_legacy_extrema'));
    assert(lines.includes('history_extrema_unavailable'));
    trimEnvelopeData(cpu, 1);
    trimEnvelopeData(connections, 1);
    assert.equal(connections.data[0].extremaSource, 'current');
    assert.deepEqual(connections.$kulaEnvelope, [2, 4]);
    assert.doesNotMatch(chartCSV(chart), /legacy|unavailable/);
});

test('raw windows do not report unavailable Min/Max that they never offered', () => {
    const raw = { ts: new Date(1000).toISOString(), extrema_profile: 'none', data: { cpu: { total: { usage: 5 } } } };
    const [item] = annotateHistoryItems([raw], {
        valid_aggregations: ['data'], available_aggregations: ['data'], extrema_profiles: { none: [] },
    });
    const lines = historyTooltipLines(historyItemContext(item), {
        aggregation: 'max', extremaSources: ['unavailable'],
    });
    assert(!lines.includes('history_extrema_unavailable'));
    assert(lines.includes('representative: raw_or_stored_value'));
});

test('unknown and missing compatibility profiles cannot fall back to copied extrema', () => {
    const sample = { ts: new Date(1000).toISOString(), data: { value: 3 }, min: { value: 2 }, max: { value: 2 } };
    for (const profile of ['none', 'unknown', undefined]) {
        const [item] = annotateHistoryItems([{ ...sample, extrema_profile: profile }], {
            valid_aggregations: ['data'], available_aggregations: ['data', 'min', 'max'],
            extrema_profiles: { none: [] },
        });
        assert.equal(historyItemExtrema(item, ['data', 'min', 'max']).maximum, null);
    }
    assert.equal(historyItemExtrema(sample, ['data']).maximum, null);
    assert.equal(historyItemExtrema(sample, ['data', 'min', 'max']).maximum, sample.max);
    const [malicious] = annotateHistoryItems([{ ...sample, extrema_profile: 'legacy' }], {
        extrema_profiles: { legacy: ['__proto__.polluted', 'constructor.prototype.polluted'] },
    });
    assert.deepEqual(Object.keys(historyItemExtrema(malicious).maximum), []);
    assert.equal({}.polluted, undefined);
});

test('partial extrema profiles copy only own scalar fields into dictionaries without prototypes', () => {
    const block = Object.assign(Object.create({ inherited: 99 }), {
        cpu: { total: Object.assign(Object.create({ steal: 99 }), { usage: 5, user: 2, system: 3 }) },
        mem: { used: 100 },
        net: { connections: 10 },
        ...JSON.parse('{"__proto__":{"polluted":1},"constructor":{"prototype":{"polluted":1}}}'),
    });
    const [item] = annotateHistoryItems([{ ts: new Date(1000).toISOString(), extrema_profile: 'legacy',
        data: {}, min: block, max: block }], { extrema_profiles: { legacy: [
        'cpu.total.usage', 'cpu.total.user', 'cpu.total.steal', 'inherited',
        'mem', 'net.*', '__proto__.polluted', 'constructor.prototype.polluted',
    ] } });
    const { minimum, maximum } = historyItemExtrema(item);
    for (const projected of [minimum, maximum]) {
        assert.deepEqual(JSON.parse(JSON.stringify(projected)), { cpu: { total: { usage: 5, user: 2 } } });
        for (const object of [projected, projected.cpu, projected.cpu.total]) {
            assert.equal(Object.getPrototypeOf(object), null);
        }
    }
    assert.notEqual(minimum.cpu.total, maximum.cpu.total);
    assert.equal(block.cpu.total.system, 3, 'projection leaves the source intact');
    assert.equal({}.polluted, undefined);
});

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

test('picker times follow the UI language clock and read typed text back', () => {
    assert.equal(formatTimeOfDay('16:57:33', 'pl'), '16:57:33');
    assert.match(formatTimeOfDay('16:57:33', 'en'), /^04:57:33\sPM$/u);
    assert.equal(formatTimeOfDay('16:57:33', 'id'), '16.57.33');
    // Every locale's own text, digits and day periods parse back exactly.
    for (const lang of ['ar', 'bn', 'de', 'en', 'hi', 'id', 'ja', 'ko', 'ms', 'pl', 'ur', 'zh']) {
        for (const time of ['00:00:00', '00:05:09', '12:00:00', '16:57:33', '23:59:59.999']) {
            assert.equal(parseTimeOfDay(formatTimeOfDay(time, lang), lang), time, `${lang} ${time}`);
        }
    }
    for (const [text, lang, want] of [
        ['14:30', 'pl', '14:30:00'], ['1430', 'pl', '14:30:00'], ['930', 'en', '09:30:00'],
        ['143005', 'pl', '14:30:05'], ['9', 'pl', '09:00:00'], ['2:30 pm', 'en', '14:30:00'],
        ['2:30 p.m.', 'pl', '14:30:00'], ['12 am', 'en', '00:00:00'], ['12:15 PM', 'en', '12:15:00'],
        ['16.57', 'id', '16:57:00'], ['16:57:33,5', 'pl', '16:57:33.500'], [' 07:05 ', 'de', '07:05:00'],
    ]) {
        assert.equal(parseTimeOfDay(text, lang), want, `${lang} ${JSON.stringify(text)}`);
    }
    for (const text of ['', 'abc', '24:00', '12:60', '12:00:60', '13 pm', '1234567', '1:2:3:4567', '10 xm']) {
        assert.equal(parseTimeOfDay(text, 'en'), null, JSON.stringify(text));
    }
    // Up/Down change the part at the caret and wrap within the day.
    assert.deepEqual(stepTimeOfDay('23:59:59', 7, 1, 'pl'), { value: '00:00:00', text: '00:00:00', start: 6, end: 8 });
    assert.deepEqual(stepTimeOfDay('10:00:00', 4, -1, 'pl'), { value: '09:59:00', text: '09:59:00', start: 3, end: 5 });
    assert.equal(stepTimeOfDay('10:00:00', 0, 1, 'en').value, '11:00:00');
    const period = formatTimeOfDay('10:00:00', 'en').length;
    assert.equal(stepTimeOfDay('10:00:00', period, 1, 'en').value, '22:00:00');
    assert.equal(stepTimeOfDay('', 0, 1, 'en'), null);
});

test('the picker clock labels its dial and wheels on the UI language clock', () => {
    const english = clockFace('en');
    assert.deepEqual(english.periods, ['AM', 'PM']);
    assert.equal(english.periodFirst, false);
    assert.deepEqual([english.hours[0], english.hours[12], english.hours[13], english.sixty[5]], ['12', '12', '1', '05']);
    assert.match(english.name(14 * 3600000 + 300000), /^2:05\sPM$/u);
    const polish = clockFace('pl');
    assert.equal(polish.periods, null);
    assert.deepEqual([polish.hours[0], polish.hours[13], polish.separator, polish.name(14 * 3600000 + 300000)],
        ['00', '13', ':', '14:05']);
    assert.equal(clockFace('id').separator, '.');
    // Korean names the day period before the hour.
    assert.equal(clockFace('ko').periodFirst, true);
    // Day-period words and digits vary with the runtime's locale data, so the
    // labels are compared with the time field's own text, not fixed strings.
    for (const lang of ['ar', 'bn', 'de', 'en', 'hi', 'id', 'ja', 'ko', 'ms', 'pl', 'ur', 'zh']) {
        const face = clockFace(lang);
        const parts = time => timeOfDayParts(time, lang);
        const text = (time, type) => parts(time).find(part => part.type === type)?.value;
        const types = parts('16:05:09').map(part => part.type);
        const twelve = types.includes('dayPeriod');
        assert.deepEqual(face.periods, twelve ? [text('04:00:00', 'dayPeriod'), text('16:00:00', 'dayPeriod')] : null, lang);
        assert.equal(face.periodFirst, twelve && types.indexOf('dayPeriod') < types.indexOf('hour'), lang);
        assert.equal(face.separator, parts('16:05:09')[types.indexOf('minute') - 1].value, lang);
        assert.equal(face.sixty[59], text('00:59:00', 'minute'), lang);
        assert.equal(face.hours.length, 24, lang);
        assert.equal(new Set(face.hours).size, twelve ? 12 : 24, lang);
        assert.equal(new Set(face.sixty).size, 60, lang);
        assert.ok(!twelve || (face.periods.every(Boolean) && face.periods[0] !== face.periods[1]), lang);
    }
});

test('a local time skipped by daylight saving time reads as the time it loads', () => {
    const zone = process.env.TZ;
    try {
        process.env.TZ = 'Europe/Warsaw';
        assert.equal(existingDateTimeInput('2026-03-29T02:30:00'), '2026-03-29T03:30:00');
        assert.equal(existingDateTimeInput('2026-03-29T01:59:59'), '2026-03-29T01:59:59');
        assert.equal(existingDateTimeInput('2026-10-25T02:30:00'), '2026-10-25T02:30:00',
            'a repeated autumn hour exists');
        assert.equal(existingDateTimeInput('2026-03-29T02:30:00', 'utc'), '2026-03-29T02:30:00');
        process.env.TZ = 'America/Santiago';
        assert.equal(existingDateTimeInput('2024-09-08T00:00:00'), '2024-09-08T01:00:00',
            'midnight itself can be skipped');
        assert.equal(existingDateTimeInput('not a date'), null);
    } finally {
        if (zone === undefined) delete process.env.TZ;
        else process.env.TZ = zone;
    }
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
    assert.ok(lines.includes('source: tier 1 · 1s'));
    assert.ok(lines.includes('output_resolution: 1m'));
    assert.ok(lines.includes('contributors: 42 source_records'));
    assert.ok(lines.includes('coverage: 87.5% · partial_range'));
    assert.ok(lines.includes('representative: policy_center'));
    assert.ok(lines.includes('range_band: bucket_minimum–bucket_maximum'));
});

test('metric numbers are rounded without exponent notation', () => {
    assert.equal(formatMetricNumber(1.4199999570846558), '1.42');
    assert.equal(formatMetricNumber(29261729792), '29261729792');
    assert.equal(formatMetricNumber(12.3456), '12.35');
    assert.equal(formatMetricNumber(0.00123456), '0.00123');
    assert.equal(formatMetricNumber(0.0000001), '0.0000001');
    assert.equal(formatMetricNumber(1e21), '1000000000000000000000');
    assert.equal(formatMetricNumber(Number.NaN), '—');
});

test('chart envelope storage stays aligned through raw points, gaps, trimming, and clearing', () => {
    const dataset = { data: [] };
    appendEnvelopePoint(dataset, new Date(1000), 5, null, null);
    assert.deepEqual(dataset.data, [{ x: 1000, y: 5 }], 'chart timestamps use numeric milliseconds');
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

test('unparsed chart points preserve zero and normalize missing or nonfinite readings to null', () => {
    const dataset = { data: [] };
    [0, undefined, null, NaN, Infinity, -Infinity].forEach((value, index) => {
        appendEnvelopePoint(dataset, new Date(index * 1000), value, null, null);
    });
    assert.deepEqual(dataset.data, [
        { x: 0, y: 0 }, { x: 1000, y: null }, { x: 2000, y: null },
        { x: 3000, y: null }, { x: 4000, y: null }, { x: 5000, y: null },
    ]);
    appendEnvelopePoint(dataset, new Date(NaN), 99, 98, 100);
    assert.equal(dataset.data.length, 6, 'invalid timestamps never reach an unparsed scale');
    assert.equal(dataset.$kulaValueCount, 1);
});

test('unavailable readings cannot be restored from extrema in charts, bands, or CSV', () => {
    for (const aggregation of ['avg', 'min', 'max']) {
        const dataset = { label: 'Seconds Behind', data: [] };
        appendEnvelopePoint(dataset, 1000, 0, 0, 0, null, aggregation, 'current');
        appendEnvelopePoint(dataset, 2000, null, -1, -1, null, aggregation, 'current');
        appendEnvelopePoint(dataset, 3000, 5, 2, 8, null, aggregation, 'current');
        assert.equal(dataset.data[0].y, 0, 'zero lag remains an observation');
        assert.equal(dataset.data[1].y, null, 'the unavailable lag remains a gap');
        assert.deepEqual(dataset.$kulaEnvelope, [0, 0, null, null, 2, 8]);
        assert.equal(dataset.$kulaValueCount, 2);
        const csv = chartCSV({ data: { datasets: [dataset] } });
        assert(csv.split('\r\n').includes('"1970-01-01T00:00:02.000Z","","",""'), 'CSV leaves the unavailable reading and extrema blank');
    }
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
            { x: 1000, y: 1.4199999570846558 },
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
        formatNumber: formatMetricNumber,
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
    assert.match(csv, /"1\.42"/, 'CSV rounds binary floating-point noise');
    assert.doesNotMatch(csv, /\d[eE][+-]\d/, 'CSV avoids exponent notation');
    assert.match(chartCursorText(chart, 1000, options), /=CPU: 1\.42%/,
        'table/cursor value formatting rounds before applying axis units');
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

test('only native raw views accept live samples, whatever the point budget predicts', () => {
    const raw = { tier: 0, resolution: '1s', sourceResolution: '1s', availableAggregations: ['data'] };
    assert.equal(historyViewAcceptsLiveSamples(raw), true);
    assert.equal(liveHistoryRefreshInterval(300, 534, 1000, raw), 0);

    // 300 points look like 1.003s per point, but an unaligned five-minute
    // window spans 301 one-second buckets, so the server answers in 2s.
    const bucketed = { tier: 0, resolution: '2s', sourceResolution: '1s',
        availableAggregations: ['data', 'min', 'max'] };
    assert.equal(historyViewAcceptsLiveSamples(bucketed), false);
    assert.equal(liveHistoryRefreshInterval(300, 300, 1000, bucketed), 2000);

    // A native-step density reduction still carries extrema that live
    // samples cannot supply.
    const dense = { ...raw, availableAggregations: ['data', 'min', 'max'] };
    assert.equal(liveHistoryRefreshInterval(300, 301, 1000, dense), 1000);

    const coarse = { tier: 1, resolution: '1m', sourceResolution: '1m', availableAggregations: ['data'] };
    assert.equal(liveHistoryRefreshInterval(300, 534, 1000, coarse), 60000);

    // A slow collector's native view keeps streaming; long windows keep their
    // display-step cadence.
    const slow = { tier: 0, resolution: '5s', sourceResolution: '5s', availableAggregations: ['data'] };
    assert.equal(liveHistoryRefreshInterval(300, 534, 5000, slow), 0);
    assert.equal(liveHistoryRefreshInterval(86400, 720, 1000, bucketed),
        liveHistoryRefreshInterval(86400, 720, 1000));
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

const { chartImageFilename, chartImageSlug, fitText, paintChartImage } =
    await importSource('../static/js/app/chart-image.js');

function recordingCanvas() {
    const calls = [];
    const ctx = {
        calls,
        setTransform: (...args) => calls.push({ op: 'setTransform', args }),
        fillRect: (...args) => calls.push({ op: 'fillRect', args, fill: ctx.fillStyle }),
        fillText: (text, x, y) => calls.push({ op: 'fillText', text, x, y, align: ctx.textAlign, font: ctx.font, fill: ctx.fillStyle }),
        drawImage: (...args) => calls.push({ op: 'drawImage', args }),
        // Seven pixels per character keeps truncation arithmetic predictable.
        measureText: text => ({ width: Array.from(text).length * 7 }),
    };
    return { width: 0, height: 0, getContext: () => ctx, ctx };
}

const imageFonts = {
    title: { font: '600 13.6px Inter', size: 13.6, color: '#f1f5f9' },
    detail: { font: '400 12px monospace', size: 12, color: '#94a3b8' },
    footer: { font: '400 12px monospace', size: 12, color: '#94a3b8' },
};

test('chart image filenames are safe, readable and time-zone explicit', () => {
    const utcDate = new Date(Date.UTC(2026, 8, 29, 14, 30, 5));
    assert.equal(chartImageFilename({ title: 'CPU Usage', hostname: 'web-01.example', date: utcDate, utc: true }),
        'kula-web-01-example-cpu-usage-20260929-143005Z.png');
    assert.equal(chartImageFilename({ title: 'Disk I/O', date: new Date(2026, 0, 2, 3, 4, 5) }),
        'kula-disk-i-o-20260102-030405.png');
    assert.equal(chartImageFilename({ title: '', hostname: '../../', date: utcDate, utc: true }),
        'kula-chart-20260929-143005Z.png');
    assert.equal(chartImageSlug('Использование ЦП'), 'использование-цп');
    assert.equal(chartImageSlug('ＣＰＵ　温度'), 'cpu-温度');
    assert.equal(chartImageSlug(`${'a'.repeat(79)} tail`).length, 79, 'trailing separator is trimmed after truncation');
    assert.doesNotMatch(chartImageFilename({ title: 'a/b\\c:d*e?"f<g>h|i', hostname: 'x' }), /[\\/:*?"<>|]/);
});

test('chart image text truncates without splitting code points', () => {
    const { ctx } = recordingCanvas();
    assert.equal(fitText(ctx, 'short', 100), 'short');
    assert.equal(fitText(ctx, 'abcdefghij', 35), 'abcd…');
    assert.equal(fitText(ctx, '😀😀😀😀😀', 21), '😀😀…');
    assert.equal(fitText(ctx, 'abcdef', 5), '');
    assert.equal(fitText(ctx, '', 100), '');
});

test('chart image composites the canvas 1:1 on an opaque themed background', () => {
    const target = recordingCanvas();
    const source = { width: 800, height: 440 };
    paintChartImage(target, {
        source, ratio: 2, title: 'CPU Usage', details: '12.5% · 4 cores',
        footerStart: 'web-01', footerEnd: '29 Sept 2026, 14:00:00 UTC – 29 Sept 2026, 14:05:00 UTC',
        background: ['#0a0e17', 'rgba(17, 24, 39, 0.85)'], border: 'rgba(55, 65, 81, 0.5)', fonts: imageFonts,
    });
    const { calls } = target.ctx;
    assert.deepEqual(calls[0], { op: 'setTransform', args: [2, 0, 0, 2, 0, 0] });
    assert.equal(target.width, (400 + 32) * 2, 'image keeps the source device-pixel density');
    const fills = calls.filter(call => call.op === 'fillRect');
    assert.deepEqual(fills.slice(0, 2).map(call => call.fill), ['#0a0e17', 'rgba(17, 24, 39, 0.85)']);
    fills.slice(0, 2).forEach(call => assert.deepEqual(call.args, [0, 0, 432, target.height / 2]));

    const draw = calls.find(call => call.op === 'drawImage');
    assert.equal(draw.args[0], source);
    assert.deepEqual(draw.args.slice(3), [400, 220], 'chart is drawn at its CSS size');
    const [, chartX, chartY] = draw.args;
    assert.equal(chartX, 16);

    const texts = calls.filter(call => call.op === 'fillText');
    const title = texts.find(call => call.text === 'CPU Usage');
    assert.equal(title.align, 'left');
    assert.ok(title.y < chartY, 'title is above the chart');
    const details = texts.find(call => call.text === '12.5% · 4 cores');
    assert.ok(details.x > title.x + 9 * 7, 'details follow the title');
    // Host and range do not fit side by side in 400px, so both are kept on
    // separate rows instead of dropping the hostname.
    const footer = texts.filter(call => call.y > chartY + 220);
    assert.deepEqual(footer.map(call => [call.text, call.align]), [
        ['web-01', 'left'],
        ['29 Sept 2026, 14:00:00 UTC – 29 Sept 2026, 14:05:00 UTC', 'left'],
    ]);
    assert.ok(footer[1].y > footer[0].y);
    footer.forEach(call => assert.ok(Array.from(call.text).length * 7 <= 400, `${call.text} overflows`));
    assert.ok(target.height / 2 >= footer[1].y + 16, 'footer keeps bottom padding');

    const wide = recordingCanvas();
    paintChartImage(wide, {
        source: { width: 1600, height: 440 }, ratio: 2, title: 'CPU Usage', details: '',
        footerStart: 'web-01', footerEnd: '29 Sept 2026, 14:00:00 UTC – 29 Sept 2026, 14:05:00 UTC',
        background: ['#fff'], border: '#ccc', fonts: imageFonts,
    });
    const wideFooter = wide.ctx.calls.filter(call => call.op === 'fillText' && call.text !== 'CPU Usage');
    assert.deepEqual(wideFooter.map(call => call.align), ['left', 'right'], 'wide footer shares one row');
    assert.equal(wideFooter[0].y, wideFooter[1].y);
    assert.equal(wideFooter[1].x, 800 + 16, 'range is aligned to the right padding edge');
    assert.ok(wide.height < target.height, 'one footer row is shorter than two');
});

test('chart image mirrors for RTL and omits an empty footer', () => {
    const target = recordingCanvas();
    paintChartImage(target, {
        source: { width: 300, height: 200 }, ratio: 1, rtl: true, title: 'استخدام المعالج',
        details: '', footerStart: '', footerEnd: '',
        background: ['#fff'], border: '#ccc', fonts: imageFonts,
    });
    const texts = target.ctx.calls.filter(call => call.op === 'fillText');
    assert.equal(texts.length, 1);
    assert.equal(texts[0].align, 'right');
    assert.equal(texts[0].x, 332 - 16, 'RTL title starts at the right padding edge');
    const draw = target.ctx.calls.find(call => call.op === 'drawImage');
    assert.equal(target.height, draw.args[2] + 200 + 16, 'no footer row is reserved');
});

const { tvGridShape } = await importSource('../static/js/app/tv-mode.js');

test('TV mode grid keeps time axes wide and fits every card on screen', () => {
    const shape = (count, width, height) => {
        const { columns, rows } = tvGridShape(count, width, height, 14);
        return `${columns}x${rows}`;
    };
    // 1080p minus the slim header and grid padding.
    assert.deepEqual([1, 2, 3, 4, 6, 9, 12, 17].map(count => shape(count, 1878, 980)),
        ['1x1', '1x2', '2x2', '2x2', '2x3', '3x3', '3x4', '4x5']);
    assert.equal(shape(4, 1040, 1800), '1x4', 'a portrait display stacks full-width charts');
    assert.equal(shape(8, 1040, 1800), '2x4');

    for (const [width, height] of [[1878, 980], [1040, 1800], [320, 480]]) {
        for (let count = 1; count <= 40; count++) {
            const { columns, rows } = tvGridShape(count, width, height, 14);
            assert.ok(columns * rows >= count, `${count} cards fit ${columns}x${rows}`);
            assert.ok(columns * (rows - 1) < count, `${columns}x${rows} leaves no empty row for ${count}`);
        }
    }

    // Rows cannot shrink below the stylesheet's 10rem floor, so a short
    // display takes fewer, wider rows instead of running off the screen.
    assert.deepEqual(tvGridShape(16, 1920, 400, 12, 160), { columns: 8, rows: 2 });
    for (const [width, height, floor] of [[1878, 980, 160], [1878, 980, 280], [1920, 400, 160], [320, 480, 160]]) {
        for (let count = 1; count <= 40; count++) {
            const { columns, rows } = tvGridShape(count, width, height, 14, floor);
            assert.ok(columns * rows >= count, `${count} cards fit ${columns}x${rows}`);
            assert.ok(rows === 1 || rows * floor + 14 * (rows - 1) <= height,
                `${columns}x${rows} rows of ${floor}px fit ${height}px`);
        }
    }
    assert.equal(tvGridShape(12, 1878, 980, 14, 160).rows, tvGridShape(12, 1878, 980, 14).rows,
        'a floor that is not reached changes nothing');

    assert.deepEqual(tvGridShape(0, 1000, 500), { columns: 1, rows: 1 });
    assert.deepEqual(tvGridShape(5, 0, 0), { columns: 1, rows: 5 }, 'an unlaid-out grid stacks');
    assert.deepEqual(tvGridShape(3, Number.NaN, 400), { columns: 1, rows: 3 });
});
