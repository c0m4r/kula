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
} = await importSource('../static/js/app/history-request.js');
const {
    annotateHistoryItems,
    historyItemContext,
    historyItemExtrema,
    insertHistoryGaps,
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
        const samples = response.samples ?? [];
        assert.ok(Array.isArray(samples), 'samples must be an array or null');
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

test('every resolution the server formats parses in the dashboard', () => {
    assert.ok(fixture.resolutions.length > 20, 'fixture lists the server resolutions');
    for (const { text, ms } of fixture.resolutions) {
        assert.equal(resolutionMilliseconds(text), ms, `live refresh cadence: ${text}`);
        assert.equal(gapResolutionMilliseconds(text), ms, `gap detection: ${text}`);
        assert.equal(minimumZoomSpan(text, 1), 12 * ms, `minimum zoom span: ${text}`);
    }
});
