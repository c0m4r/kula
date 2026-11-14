// Run by internal/storage/history_contract_test.go with real /api/history
// responses. Each scenario states what the dashboard must conclude from the
// response; the dashboard's own modules must reach that conclusion.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

async function importSource(relativePath) {
    const source = await readFile(new URL(relativePath, import.meta.url), 'utf8');
    const encoded = Buffer.from(source).toString('base64');
    return import(`data:text/javascript;base64,${encoded}`);
}

const {
    historyViewAcceptsLiveSamples,
    liveHistoryRefreshInterval,
    resolutionMilliseconds,
    rollingTailRequest,
    rollingTailSource,
} = await importSource('../static/js/app/history-request.js');
const {
    annotateHistoryItems,
    historyItemContext,
    historyItemExtrema,
    historyViewSignature,
    insertHistoryGaps,
    mergeHistoryTail,
    resolutionMilliseconds: gapResolutionMilliseconds,
} = await importSource('../static/js/app/history-data.js');
const { minimumZoomSpan } = await importSource('../static/js/app/history-navigation.js');
const { appendEnvelopePoint } = await importSource('../static/js/app/chart-envelope.js');

if (!process.argv[2]) throw new Error('usage: node history_contract_test.mjs <fixture.json>');
const fixture = JSON.parse(await readFile(process.argv[2], 'utf8'));

// The same provenance the dashboard derives for every item of a response;
// charts-data.js builds its live-refresh view from these fields.
function responseView(response) {
    const [probe] = annotateHistoryItems([{ ts: response.requested_to, data: {} }], response);
    const { tier, resolution, sourceResolution, availableAggregations } = historyItemContext(probe).source;
    return { tier, resolution, sourceResolution, availableAggregations: [...availableAggregations] };
}

function valueAt(block, path) {
    return path.split('.').reduce((value, key) => value?.[key], block);
}

function hasFiniteLeaf(value) {
    if (typeof value === 'number') return Number.isFinite(value);
    return !!value && typeof value === 'object' && Object.values(value).some(hasFiniteLeaf);
}

for (const scenario of fixture.scenarios) {
    test(scenario.name, () => {
        const { request, expect, response } = scenario;
        const from = Date.parse(request.from);
        const to = Date.parse(request.to);
        const samples = response.samples;
        assert.ok(Array.isArray(samples), 'samples must be an array, empty when the window holds none');
        assert.ok(samples.length <= request.points, 'response exceeds the requested point budget');

        const view = responseView(response);
        const sourceMs = resolutionMilliseconds(view.sourceResolution);
        assert.ok(resolutionMilliseconds(view.resolution) > 0 && sourceMs > 0,
            `unparseable resolution ${view.resolution}/${view.sourceResolution}`);
        assert.deepEqual(view.availableAggregations, expect.aggregations, 'aggregation selector');

        // Stream live samples only onto views that already hold them.
        assert.equal(historyViewAcceptsLiveSamples(view), expect.live, 'live streaming decision');
        const refresh = liveHistoryRefreshInterval((to - from) / 1000, request.points,
            request.live_interval_ms, view);
        assert.equal(refresh === 0, expect.live, `refresh interval ${refresh}ms`);

        const items = annotateHistoryItems(samples, response);
        let previous = -Infinity;
        const spacing = [];
        for (const item of items) {
            const context = historyItemContext(item);
            assert.ok(Number.isFinite(context.timestamp) && context.timestamp > previous,
                'observations must be chronological');
            assert.ok(context.timestamp > from && context.timestamp <= to, 'observation outside the request');
            assert.ok(context.bucketStart < context.bucketEnd, 'empty bucket bounds');
            assert.ok(context.coverage === null || (context.coverage >= 0 && context.coverage <= 1), 'coverage');
            if (Number.isFinite(previous)) spacing.push(context.timestamp - previous);
            previous = context.timestamp;
        }
        if (expect.live && spacing.length > 2) {
            spacing.sort((a, b) => a - b);
            assert.ok(spacing[Math.floor(spacing.length / 2)] <= sourceMs * 1.05,
                'a streamed view must already be at live-sample density');
        }

        // Min/Max are offered exactly when some bucket carries usable extrema.
        const offered = view.availableAggregations.includes('max');
        const extrema = items.map(item => historyItemExtrema(item, view.availableAggregations));
        assert.equal(extrema.some(({ maximum }) => hasFiniteLeaf(maximum)), offered,
            'offered aggregations disagree with the extrema buckets carry');

        // Partial profiles must name real fields, or the allowlist silently
        // stops matching the JSON it restricts.
        items.forEach((item, index) => {
            const profile = historyItemContext(item).extremaProfile;
            const fields = response.extrema_profiles?.[profile] ?? [];
            for (const field of fields.filter(path => path !== '*')) {
                assert.ok(Number.isFinite(valueAt(item.data, field)), `${profile} field ${field} missing from data`);
                if (item.max && valueAt(item.max, field) !== undefined) {
                    assert.ok(Number.isFinite(valueAt(extrema[index].maximum, field)),
                        `${profile} field ${field} was not projected`);
                }
            }
        });

        // The headline series must draw every bucket in Max; this is the
        // symptom of a live sample without extrema reaching a Max chart.
        if (offered) {
            const series = { data: [] };
            items.forEach((item, index) => {
                const { minimum, maximum, profile } = extrema[index];
                appendEnvelopePoint(series, historyItemContext(item).timestamp, item.data.cpu.total.usage,
                    minimum?.cpu?.total?.usage, maximum?.cpu?.total?.usage, null, 'max', profile);
                if (profile === 'current') {
                    assert.ok(minimum.cpu.total.usage <= item.data.cpu.total.usage &&
                        item.data.cpu.total.usage <= maximum.cpu.total.usage, 'representative value outside its extrema');
                }
            });
            assert.ok(series.data.every(point => Number.isFinite(point.y) && !point.extremaUnavailable),
                'Max left CPU usage buckets blank');
        }

        const gaps = insertHistoryGaps(items, response.resolution).filter(item => item?._gap).length;
        assert.equal(gaps, expect.gaps, 'missing-observation markers');
    });
}

// A rolling tail refresh must plan the request the server answered at the
// view's step, and splicing it onto the earlier snapshot must reproduce the
// full refresh bucket for bucket (the left edge excepted: the full window
// clips its first bucket at the new start, the buffer keeps it whole).
for (const tail of fixture.tails ?? []) {
    test(tail.name, () => {
        const { before, refresh, after } = tail;
        const beforeItems = insertHistoryGaps(annotateHistoryItems(before.samples, before), before.resolution);
        const last = historyItemContext(beforeItems.filter(item => !item._gap).at(-1));
        const plan = rollingTailRequest({
            rangeMs: tail.range_ms,
            points: tail.points,
            toMs: Date.parse(tail.tail_to),
            stepMs: resolutionMilliseconds(before.resolution),
            lastBucketStart: last.bucketStart,
        });
        assert.deepEqual(plan, { fromMs: Date.parse(tail.tail_from), points: tail.tail_points }, 'tail request');
        assert.deepEqual(rollingTailSource(plan, { tier: before.tier, downsampled: before.downsampled }),
            { tier: tail.tier, requestFromMs: Date.parse(tail.request_from) }, 'tail tier hint and request start');
        assert.notEqual(liveHistoryRefreshInterval(tail.range_ms / 1000, tail.points, 1000, {
            tier: before.tier, resolution: before.resolution, sourceResolution: before.source_resolution,
            availableAggregations: before.available_aggregations,
        }), 0, 'a tail-refreshed view is a bucketed view');
        assert.equal(historyViewSignature(refresh), historyViewSignature(before), 'tail describes the same view');
        assert.equal(historyViewSignature(after), historyViewSignature(before), 'full refresh describes the same view');

        const merged = mergeHistoryTail(beforeItems, refresh.samples, refresh, plan.fromMs, refresh.resolution);
        assert.ok(merged, 'tail merges onto the snapshot');
        const afterItems = annotateHistoryItems(after.samples, after);
        const interiorFrom = historyItemContext(afterItems[1]).bucketStart;
        const shape = item => {
            const context = historyItemContext(item);
            return [item.ts, item.bucket_start, item.bucket_end, item.sample_count, item.coverage,
                item.extrema_profile, item.data, item.min, item.max, context.source.resolution];
        };
        // Equal up to float rounding: a bucket the server merged across two
        // decode batches can differ in the last bit of a mean or extremum.
        const close = (a, b) => {
            if (typeof a === 'number' && typeof b === 'number') {
                return Math.abs(a - b) <= 1e-9 * Math.max(1, Math.abs(a), Math.abs(b));
            }
            if (a && b && typeof a === 'object' && typeof b === 'object') {
                const keys = Object.keys(a);
                return Array.isArray(a) === Array.isArray(b) && keys.length === Object.keys(b).length &&
                    keys.every(key => Object.prototype.hasOwnProperty.call(b, key) && close(a[key], b[key]));
            }
            return a === b;
        };
        const window = items => items.filter(item => !item._gap &&
            historyItemContext(item).bucketStart >= interiorFrom).map(shape);
        const mergedWindow = window(merged);
        const afterWindow = window(afterItems);
        assert.equal(mergedWindow.length, afterWindow.length, 'merged view has the full refresh\'s buckets');
        mergedWindow.forEach((bucket, index) => assert.ok(close(bucket, afterWindow[index]),
            `merged bucket ${bucket[1]} equals the full refresh`));
        assert.equal(merged.filter(item => item._gap).length,
            insertHistoryGaps(afterItems, after.resolution).filter(item => item._gap).length, 'gap markers');
    });
}

test('every resolution the server formats parses in the dashboard', () => {
    assert.ok(fixture.resolutions.length > 20, 'fixture lists the server resolutions');
    for (const { text, ms } of fixture.resolutions) {
        assert.equal(resolutionMilliseconds(text), ms, `live refresh cadence: ${text}`);
        assert.equal(gapResolutionMilliseconds(text), ms, `gap detection: ${text}`);
        assert.equal(minimumZoomSpan(text, 1), 12 * ms, `minimum zoom span: ${text}`);
    }
});
