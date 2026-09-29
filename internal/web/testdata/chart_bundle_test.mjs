// Checks the vendored Chart.js bundle (addons/build-chartjs.sh) and its
// native-Date adapter. Calendar math is local time, so pin a DST zone.
process.env.TZ = 'America/New_York';

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

import { dateAdapter as adapter } from '../../../addons/chartjs/date-adapter.js';

const local = (...parts) => new Date(...parts).getTime();
const HOUR = 3600000;

test('adapter adds calendar units in local time and fixed units in elapsed time', () => {
    assert.equal(adapter.add(local(2024, 2, 9, 12), 1, 'day'), local(2024, 2, 10, 12));
    assert.equal(adapter.add(local(2024, 2, 9, 12), 1, 'day') - local(2024, 2, 9, 12), 23 * HOUR);
    assert.equal(adapter.add(local(2024, 2, 10, 1), 1, 'hour'), local(2024, 2, 10, 1) + HOUR);
    assert.equal(adapter.add(local(2024, 0, 31, 12), 1, 'month'), local(2024, 1, 29, 12));
    assert.equal(adapter.add(local(2024, 0, 31, 12), 1, 'quarter'), local(2024, 3, 30, 12));
    assert.equal(adapter.add(local(2024, 1, 29, 12), 1, 'year'), local(2025, 1, 28, 12));
    assert.equal(adapter.add(local(2024, 0, 1), 2, 'week'), local(2024, 0, 15));
    // 01:30 on Nov 3 2024 occurs twice; a zero step keeps the later one.
    const ambiguous = local(2024, 10, 3, 1, 30) + HOUR;
    assert.equal(adapter.add(ambiguous, 0, 'day'), ambiguous);
    assert.ok(Number.isNaN(adapter.add(local(2024, 0, 1), NaN, 'day')));
});

test('adapter aligns to local unit starts and ends', () => {
    const t = local(2024, 4, 15, 13, 45, 30, 250); // Wednesday
    assert.equal(adapter.startOf(t, 'second'), local(2024, 4, 15, 13, 45, 30));
    assert.equal(adapter.startOf(t, 'hour'), local(2024, 4, 15, 13));
    assert.equal(adapter.startOf(t, 'week'), local(2024, 4, 12));
    assert.equal(adapter.startOf(t, 'isoWeek', true), local(2024, 4, 13));
    assert.equal(adapter.startOf(t, 'quarter'), local(2024, 3, 1));
    assert.equal(adapter.startOf(t, 'year'), local(2024, 0, 1));
    assert.equal(adapter.startOf(t, 'millisecond'), t);
    assert.equal(adapter.endOf(t, 'day'), local(2024, 4, 15, 23, 59, 59, 999));
    assert.equal(adapter.endOf(t, 'week'), local(2024, 4, 18, 23, 59, 59, 999));
    assert.equal(adapter.endOf(local(2024, 1, 10), 'month'), local(2024, 1, 29, 23, 59, 59, 999));
    assert.equal(adapter.endOf(t, 'quarter'), local(2024, 5, 30, 23, 59, 59, 999));
    assert.equal(adapter.endOf(t, 'year'), local(2024, 11, 31, 23, 59, 59, 999));
});

test('adapter counts whole units', () => {
    assert.equal(adapter.diff(local(2024, 2, 10, 12), local(2024, 2, 9, 12), 'day'), 1);
    assert.equal(adapter.diff(local(2024, 2, 10, 11), local(2024, 2, 9, 12), 'day'), 0);
    assert.equal(adapter.diff(local(2024, 2, 9, 12), local(2024, 2, 10, 12), 'day'), -1);
    assert.equal(adapter.diff(local(2024, 2, 10, 12), local(2024, 2, 9, 12), 'hour'), 23);
    assert.equal(adapter.diff(local(2024, 1, 29), local(2024, 0, 31), 'month'), 1);
    assert.equal(adapter.diff(local(2024, 1, 28), local(2024, 0, 31), 'month'), 0);
    assert.equal(adapter.diff(local(2025, 0, 1), local(2024, 0, 1), 'year'), 1);
    assert.equal(adapter.diff(local(2024, 0, 22), local(2024, 0, 1), 'week'), 3);
});

test('adapter parses timestamps, Dates and ISO strings', () => {
    assert.equal(adapter.parse(1700000000000), 1700000000000);
    assert.equal(adapter.parse(new Date(1700000000000)), 1700000000000);
    assert.equal(adapter.parse('2024-05-15T12:00:00Z'), Date.UTC(2024, 4, 15, 12));
    assert.equal(adapter.parse('not a date'), null);
    assert.equal(adapter.parse(null), null);
    assert.equal(adapter.parse(true), null);
});

test('adapter formats its own and Kula\'s format tokens', () => {
    const t = local(2024, 6, 4, 15, 7, 9, 42);
    assert.equal(adapter.format(t, 'MMM d, HH:mm:ss'), 'Jul 4, 15:07:09');
    assert.equal(adapter.format(t, adapter.formats().millisecond), '3:07:09.042 PM');
    assert.equal(adapter.format(t, adapter.formats().hour), '3PM');
    assert.equal(adapter.format(t, adapter.formats().quarter), 'Q3 - 2024');
    assert.equal(adapter.format(local(2024, 0, 5, 0, 30), 'yyyy-MM-dd hh:mm a'), '2024-01-05 12:30 AM');
    assert.equal(adapter.format(NaN, 'yyyy'), '');
});

// Enough of CanvasRenderingContext2D for layout: text is 6px per character.
function stubCanvas(width, height) {
    const canvas = { width, height, style: {} };
    const values = { canvas };
    canvas.getContext = () => new Proxy(values, {
        get: (target, key) => (key in target ? target[key]
            : key === 'measureText' ? text => ({ width: String(text).length * 6 }) : () => {}),
        set: (target, key, value) => { target[key] = value; return true; },
    });
    return canvas;
}

test('bundle registers only what the dashboard uses', async () => {
    vm.runInThisContext(await readFile(new URL('../static/js/chartjs/chartjs-bundle.min.js', import.meta.url), 'utf8'));
    const { Chart } = globalThis;
    const names = registry => Object.keys(registry.items).sort();
    assert.deepEqual(names(Chart.registry.controllers), ['line']);
    assert.deepEqual(names(Chart.registry.elements), ['line', 'point']);
    assert.deepEqual(names(Chart.registry.scales), ['linear', 'time']);
    assert.deepEqual(names(Chart.registry.plugins), ['legend', 'tooltip', 'zoom']);
    assert.equal(typeof Chart.Tooltip.positioners, 'object');
    assert.equal(typeof Chart.getChart, 'function');

    // Six hours across the spring-forward gap: ticks stay on local hours.
    const start = local(2024, 2, 10, 0);
    const chart = new Chart(stubCanvas(800, 300), {
        type: 'line',
        data: { datasets: [{ data: [{ x: start, y: 1 }, { x: start + 6 * HOUR, y: 2 }] }] },
        options: {
            responsive: false,
            animation: false,
            parsing: false,
            scales: { x: { type: 'time', time: { unit: 'hour' } } },
        },
    });
    const hours = chart.scales.x.ticks.map(tick => new Date(tick.value).getHours());
    assert.deepEqual(hours, [0, 1, 3, 4, 5, 6, 7]);
    assert.ok(chart.scales.x.ticks.every(tick => tick.value % (15 * 60000) === 0));
    assert.equal(typeof chart.isZoomingOrPanning, 'function');
    chart.destroy();
});
