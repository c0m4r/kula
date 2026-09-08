window.errors = [];
window.addEventListener('error', e => errors.push(e.error?.stack || e.message));
window.addEventListener('unhandledrejection', e => errors.push(e.reason?.stack || String(e.reason)));
const check = (condition, message) => { if (!condition) throw new Error(message); };
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
const frame = () => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
const { state } = await import('./js/app/state.js');
const { i18n } = await import('./js/app/i18n.js');
const controls = await import('./js/app/controls.js');
const data = await import('./js/app/charts-data.js');
const { chartUpdates } = await import('./js/app/chart-controller.js');
const { updateUrl } = await import('./js/app/history-navigation.js');
const fetchOriginal = window.fetch.bind(window);
window.historyRequests = 0;
window.fetch = (url, options) => {
    if (String(url).includes('/api/history?')) {
        window.historyRequests++;
        if (window.gapFixture) url += '&gaps=true';
    }
    return fetchOriginal(url, options);
};
// Exercise the real entry point and listeners. Keep transport under fixture
// control so the tests can replay exact samples without a live backend.
window.WebSocket = class {
    static CONNECTING = 0;
    static OPEN = 1;
    constructor() { this.readyState = 0; window.socketCreated = true; }
    close() {}
    send() {}
};
await import('./js/app/main.js');
for (let i = 0; !window.socketCreated && i < 500; i++) await pause(10);
check(window.socketCreated, 'Dashboard bootstrap did not reach the WebSocket connection');
state.timeRange = null;
state.customTo = new Date('2026-09-05T10:00:00Z');
state.customFrom = new Date('2026-09-04T10:00:00Z');
await data.fetchCustomHistory(state.customFrom, state.customTo);
await frame();
const originalCharts = Object.values(Chart.instances);
const originalBuffer = state.dataBuffer;
const cpu = state.charts.cpu;

// Hovering may synchronize the line and tooltip, but only an explicit pin is
// allowed to consume space in the top history-information row. A drag's
// synthetic click must not leave the shared crosshair pinned.
const crosshairRect = cpu.canvas.getBoundingClientRect();
const crosshairY = crosshairRect.top + (cpu.chartArea.top + cpu.chartArea.bottom) / 2;
const crosshairStartX = crosshairRect.left + cpu.chartArea.left + 30;
const crosshairEndX = crosshairStartX + 40;
cpu.canvas.dispatchEvent(new MouseEvent('mousemove', {
    bubbles: true, clientX: crosshairStartX, clientY: crosshairY,
}));
await frame();
check(document.getElementById('pinned-time').classList.contains('hidden'),
    'Transient crosshair hover changed the top-row layout');
cpu.canvas.dispatchEvent(new PointerEvent('pointerdown', {
    bubbles: true, pointerId: 7, pointerType: 'mouse', isPrimary: true,
    button: 0, buttons: 1, clientX: crosshairStartX, clientY: crosshairY,
}));
cpu.canvas.dispatchEvent(new PointerEvent('pointermove', {
    bubbles: true, pointerId: 7, pointerType: 'mouse', isPrimary: true,
    button: 0, buttons: 1, clientX: crosshairEndX, clientY: crosshairY,
}));
cpu.canvas.dispatchEvent(new PointerEvent('pointerup', {
    bubbles: true, pointerId: 7, pointerType: 'mouse', isPrimary: true,
    button: 0, buttons: 0, clientX: crosshairEndX, clientY: crosshairY,
}));
cpu.canvas.dispatchEvent(new MouseEvent('click', {
    bubbles: true, button: 0, clientX: crosshairEndX, clientY: crosshairY,
}));
check(document.getElementById('pinned-time').classList.contains('hidden'),
    'Drag zoom pinned the crosshair');
await pause(300);
cpu.canvas.dispatchEvent(new MouseEvent('click', {
    bubbles: true, button: 0, clientX: crosshairEndX, clientY: crosshairY,
}));
check(!document.getElementById('pinned-time').classList.contains('hidden'),
    'Intentional crosshair click did not pin the timestamp');
cpu.canvas.dispatchEvent(new MouseEvent('click', {
    bubbles: true, button: 0, clientX: crosshairEndX, clientY: crosshairY,
}));
cpu.canvas.dispatchEvent(new MouseEvent('mouseleave', { bubbles: true }));

cpu.setDatasetVisibility(0, false);
const requestsBefore = window.historyRequests;
const start = performance.now();
controls.toggleLayout(); await frame();
const layoutMs = performance.now() - start;
check(originalCharts.every(chart => Chart.instances[chart.id] === chart), 'Layout replaced chart instances');
check(state.dataBuffer === originalBuffer, 'Layout replaced history buffer');
check(window.historyRequests === requestsBefore, 'Layout refetched history');
check(!cpu.isDatasetVisible(0), 'Layout lost legend visibility');

// Graph bounds copy only their known fields from stored preferences. A
// __proto__ key must not supply an inherited automatic limit to the UI.
const savedGraphBounds = localStorage.getItem('kula_graphs_max');
const boundsButton = document.querySelector('#card-network button[title="Graph Bounds"]');
const boundsDropdown = document.querySelector('#card-network .chart-settings-dropdown');
for (const fixture of [
    { stored: '{}', mode: 'off', value: 1000 },
    { stored: '{"network":{"value":123,"__proto__":{"auto":999}}}', mode: 'off', value: 123 },
    { stored: '{"network":{"value":123,"auto":456}}', mode: 'off', value: 456 },
    { stored: '{"network":{"mode":"auto","value":123,"auto":456}}', mode: 'on', value: 456 },
    { stored: '{"network":{"mode":"on","value":123}}', mode: 'on', value: 123 },
]) {
    localStorage.setItem('kula_graphs_max', fixture.stored);
    boundsButton.click();
    check(boundsDropdown.querySelector('select').value === fixture.mode &&
        Number(boundsDropdown.querySelector('input').value) === fixture.value,
        'Graph bounds adopted unexpected properties or lost a saved limit');
    boundsButton.click();
}
if (savedGraphBounds === null) localStorage.removeItem('kula_graphs_max');
else localStorage.setItem('kula_graphs_max', savedGraphBounds);

// A local raw-data zoom must retain a known interior outage rather than mark
// the new view complete merely because its outer bounds fit the buffer.
const localFrom = new Date('2026-09-04T16:00:00Z');
const localTo = new Date('2026-09-05T04:00:00Z');
check(!data.tryZoomFromBuffer(localFrom, localTo), 'Direct buffer zoom accepted downsampled history');
state.currentTier = 0;
state.currentDownsampled = false;
const middle = Math.floor(state.dataBuffer.length / 2);
state.dataBuffer.splice(middle, 0, { ts: state.dataBuffer[middle].ts, _gap: true });
check(data.tryZoomFromBuffer(localFrom, localTo), 'Covered raw buffer was not reused');
check(!state.historyCoverage.complete && state.historyStatus === 'partial',
    'Local zoom silently promoted an interior gap to complete');
state.customFrom = new Date('2026-09-04T10:00:00Z');
state.customTo = new Date('2026-09-05T10:00:00Z');
await data.fetchCustomHistory(state.customFrom, state.customTo);

// Dynamic identities retain their existing history, new series receive null
// prefixes, and a missing observation creates an explicit gap.
const sensorStart = new Date('2026-09-05T10:00:01Z');
data.addSampleToCharts({ ts: sensorStart.toISOString(), data: {
    ts: sensorStart.toISOString(), cpu: { sensors: [{ name: 'Package', value: 40 }] },
} }, sensorStart);
const packageSeries = state.charts.cputemp.data.datasets.find(dataset => dataset.label === 'Package');
const sensorNext = new Date(sensorStart.getTime() + 1000);
data.addSampleToCharts({ ts: sensorNext.toISOString(), data: {
    ts: sensorNext.toISOString(), cpu: { sensors: [
        { name: 'Core', value: 45 }, { name: 'Package', value: 41 },
    ] },
} }, sensorNext);
const coreSeries = state.charts.cputemp.data.datasets.find(dataset => dataset.label === 'Core');
check(state.charts.cputemp.data.datasets.includes(packageSeries), 'Sensor reorder discarded retained history');
check(coreSeries.data[0].y === null, 'New sensor was not null-backfilled');
const sensorMissing = new Date(sensorNext.getTime() + 1000);
data.addSampleToCharts({ ts: sensorMissing.toISOString(), data: {
    ts: sensorMissing.toISOString(), cpu: {}, apps: {},
} }, sensorMissing);
check(packageSeries.data.at(-1).y === null && coreSeries.data.at(-1).y === null,
    'Missing sensors were connected across an outage');
check(state.charts.nginxConn.data.datasets[0].data.at(-1).y === null,
    'Missing application data was connected across an outage');
await data.fetchCustomHistory(state.customFrom, state.customTo);
document.getElementById('card-cpu-temp').classList.add('hidden');

// Tier 0 can still be downsampled. Its aggregation controls must remain
// visible and zooming must fetch finer data instead of reusing the coarse buffer.
const normalFetch = window.fetch;
window.fetch = async (url, options) => {
    const response = await normalFetch(url, options);
    if (!String(url).includes('/api/history?')) return response;
    const payload = await response.json();
    payload.tier = 0;
    payload.resolution = '10s';
    payload.source_resolution = '1s';
    payload.downsampled = true;
    payload.complete = true;
    payload.exact_complete = true;
    return new Response(JSON.stringify(payload), {
        status: response.status,
        headers: { 'Content-Type': 'application/json' },
    });
};
state.defaultAggregation = 'avg';
state.currentAggregation = 'max';
state.customFrom = new Date('2026-09-05T08:00:00Z');
state.customTo = new Date('2026-09-05T10:00:00Z');
await data.fetchCustomHistory(state.customFrom, state.customTo);
check(state.currentTier === 0 && state.currentDownsampled, 'Tier-0 downsampling metadata was lost');
check(!document.getElementById('btn-agg-menu').classList.contains('hidden'),
    'Tier-0 downsampling hid valid aggregation controls');
updateUrl(state);
check(new URL(location.href).searchParams.get('agg') === 'max',
    'Share URL discarded a valid tier-0 aggregation');
const zoomRequests = window.historyRequests;
cpu.options.scales.x.min = state.customFrom.getTime() + 600000;
cpu.options.scales.x.max = state.customTo.getTime() - 600000;
data.syncZoom({ chart: cpu, complete: true });
for (let i = 0; window.historyRequests === zoomRequests && i < 100; i++) await pause(10);
check(window.historyRequests > zoomRequests, 'Downsampled tier-0 zoom reused coarse buffered data');
for (let i = 0; state.loadingHistory && i < 500; i++) await pause(10);
window.fetch = normalFetch;
state.currentAggregation = 'avg';
state.customFrom = new Date('2026-09-04T10:00:00Z');
state.customTo = new Date('2026-09-05T10:00:00Z');
updateUrl(state);
await data.fetchCustomHistory(state.customFrom, state.customTo);

// Reapplying translations must render the active preset, not the static 5m
// key baked into the initial HTML.
state.timeRange = 3600;
controls.syncTimeRangeUI(3600);
await i18n.loadTranslations('pl');
i18n.applyTranslations();
document.dispatchEvent(new Event('kula-i18n-changed'));
check(i18n.t('last_1_h') !== 'Last 1h', 'Fixture did not load the Polish locale');
check(document.getElementById('time-range-display').textContent === i18n.t('last_1_h'),
    'Language change reset the active range label to five minutes');
check(state.charts.loadavg.data.datasets[0].label === i18n.t('1_min') &&
    state.charts.loadavg.data.datasets[0].label !== '1_min',
    'Loaded translations did not refresh chart legend labels');
await i18n.loadTranslations('en');
i18n.applyTranslations();
document.dispatchEvent(new Event('kula-i18n-changed'));
state.timeRange = null;
state.customFrom = new Date('2026-09-04T10:00:00Z');
state.customTo = new Date('2026-09-05T10:00:00Z');
controls.syncCustomRangeUI(state.customFrom, state.customTo);

// Slow live observations must teach the cadence even when a newer history
// response already covered them. This avoids a permanent 1s refresh estimate.
const previousLatest = state.lastSample;
state.lastSample = { ts: '2026-09-06T00:00:00Z' };
state.lastLiveSampleTs = null;
state.liveSampleIntervals = [];
state.liveSampleIntervalMs = null;
for (let i = 0; i < 6; i++) {
    data.pushLiveSample({ ts: new Date(Date.parse('2026-09-05T10:00:00Z') + i * 5000).toISOString() });
}
check(state.liveSampleIntervalMs === 5000, 'Slow collection interval was pinned to 1s');
state.lastSample = previousLatest;
state.lastLiveSampleTs = null;
state.liveSampleIntervals = [];
state.liveSampleIntervalMs = null;
data.updateAllCharts();
await frame();
check(!document.querySelector('.btn-chart-data'), 'Data controls enabled by default');
check(!document.querySelector('.chart-data-panel'), 'Data panel created without opt-in');
check(cpu.canvas.hasAttribute('aria-describedby') && cpu.canvas.tabIndex === 0, 'Basic accessibility is missing');
const checkbox = document.getElementById('set-chart-data-controls');
checkbox.checked = true; checkbox.dispatchEvent(new Event('change', { bubbles: true }));
check(document.querySelector('.btn-chart-data'), 'Data opt-in failed');
check(!document.querySelector('.chart-data-panel'), 'Opt-in eagerly created data panels');
cpu.canvas.closest('.chart-card').querySelector('.btn-chart-data').click();
check(document.querySelectorAll('.chart-data-table tbody tr').length === 50, 'Table preview is not bounded to 50 rows');
check(document.querySelector('.chart-data-download'), 'CSV control missing');
checkbox.checked = false; checkbox.dispatchEvent(new Event('change', { bubbles: true }));
check(!document.querySelector('.btn-chart-data') && !document.querySelector('.chart-data-panel'), 'Opt-out retained controls or table');
check(JSON.parse(localStorage.getItem('kula_ui_settings')).chart_data_controls === false, 'Opt-out did not persist');

const tooltipPoint = cpu.data.datasets[1].data[0];
const tooltipItems = [{ parsed: { x: Number(tooltipPoint.x) }, raw: tooltipPoint }];
check(cpu.options.plugins.tooltip.callbacks.footer(tooltipItems).length === 0, 'Detailed tooltips enabled by default');
const tooltipDetails = document.getElementById('set-chart-tooltip-details');
tooltipDetails.checked = true;
tooltipDetails.dispatchEvent(new Event('change', { bubbles: true }));
check(cpu.options.plugins.tooltip.callbacks.footer(tooltipItems).length > 0, 'Tooltip details opt-in failed');
const { applyTheme } = await import('./js/app/settings.js');
state.theme = 'light'; applyTheme(); await frame();
check(cpu.options.plugins.tooltip.footerColor === cpu.options.plugins.tooltip.bodyColor, 'Tooltip footer does not follow light theme');
check(cpu.options.plugins.tooltip.footerColor !== '#fff', 'Tooltip footer stayed white in light theme');
state.theme = 'dark'; applyTheme(); await frame();
tooltipDetails.checked = false;
tooltipDetails.dispatchEvent(new Event('change', { bubbles: true }));

// Exercise the picker through its UI, including validation before requests.
check(cpu.scales.x.labelRotation === 0, 'Time-axis labels are rotated');
check(document.getElementById('sampling-info').textContent.includes('Tier 2'), 'Sampling info lost its tier after loading');
const picker = document.getElementById('time-custom');
// Twelve-hour labels including seconds must retain a readable gap on a
// screenshot-sized card, not merely avoid literal character overlap.
const oldAxis = { min: cpu.options.scales.x.min, max: cpu.options.scales.x.max,
    unit: cpu.options.scales.x.time.unit };
const oldLocale = i18n.currentLang;
i18n.currentLang = 'en-US';
cpu.options.scales.x.min = Date.parse('2026-09-05T20:43:00Z');
cpu.options.scales.x.max = Date.parse('2026-09-05T20:48:00Z');
cpu.options.scales.x.time.unit = 'second';
cpu.resize(600, 250);
cpu.update('none');
const timeScale = cpu.scales.x;
check(timeScale.ticks.some(tick => /AM|PM/.test(tick.label)), 'Spacing fixture did not use AM/PM labels');
cpu.ctx.save();
cpu.ctx.font = Chart.helpers.toFont(timeScale.options.ticks.font).string;
for (let i = 1; i < timeScale.ticks.length; i++) {
    const previousWidth = cpu.ctx.measureText(timeScale.ticks[i - 1].label).width;
    const width = cpu.ctx.measureText(timeScale.ticks[i].label).width;
    const gap = timeScale.getPixelForTick(i) - timeScale.getPixelForTick(i - 1) - (previousWidth + width) / 2;
    check(gap >= 20, `AM/PM tick labels are crowded: ${gap}px`);
}
cpu.ctx.restore();
i18n.currentLang = oldLocale;
cpu.options.scales.x.min = oldAxis.min;
cpu.options.scales.x.max = oldAxis.max;
if (oldAxis.unit === undefined) delete cpu.options.scales.x.time.unit;
else cpu.options.scales.x.time.unit = oldAxis.unit;
cpu.resize();
data.updateAllCharts();
await frame();
document.getElementById('btn-settings').click();
const zoneRow = document.querySelector('.settings-time-zone');
const zoneLabelBox = zoneRow.firstElementChild.getBoundingClientRect();
const zoneActionsBox = zoneRow.querySelector('.time-zone-actions').getBoundingClientRect();
const zoneRowBox = zoneRow.getBoundingClientRect();
check(zoneLabelBox.right < zoneActionsBox.left &&
    Math.abs((zoneLabelBox.top + zoneLabelBox.bottom) / 2 - (zoneActionsBox.top + zoneActionsBox.bottom) / 2) < 4 &&
    zoneActionsBox.right <= zoneRowBox.right,
    'Local/UTC control is not aligned as a labelled settings row');
document.getElementById('btn-settings').click();
const pickerButton = document.getElementById('btn-custom-range');
const fromInput = document.getElementById('custom-from');
const toInput = document.getElementById('custom-to');
const { parseDateTimeInput } = await import('./js/app/format.js');
pickerButton.click();
for (let i = 0; picker.querySelector('button[type="submit"]').disabled && i < 100; i++) await pause(10);
check(pickerButton.getAttribute('aria-expanded') === 'true', 'Picker does not announce its open state');
check(parseDateTimeInput(fromInput.value, state.timeZone).getTime() === Math.floor(state.customFrom.getTime() / 1000) * 1000,
    `Picker discarded the selected range: ${fromInput.value}`);
check(fromInput.step === '1' && !fromInput.value.includes('.'), 'One-second collection exposes fractional precision');
fromInput.value = '2026-04-20T12:00:00';
toInput.value = '2026-09-02T12:00:00';
fromInput.dispatchEvent(new Event('input', { bubbles: true }));
check(document.getElementById('custom-range-error').textContent === i18n.t('range_max_31_days'),
    'Oversized range validation lost precedence');
fromInput.value = '2026-04-20T12:00:00';
toInput.value = '2026-04-21T12:00:00';
fromInput.dispatchEvent(new Event('input', { bubbles: true }));
check(document.getElementById('custom-range-error').textContent === i18n.t('range_outside_retention') &&
    picker.querySelector('button[type="submit"]').disabled,
    'Typed dates in a retention gap were accepted');
const pickerRequests = window.historyRequests;
fromInput.value = toInput.value;
picker.requestSubmit();
check(!document.getElementById('custom-range-error').classList.contains('hidden'), 'Reversed range has no validation message');
fromInput.value = '2026-07-01T12:00';
toInput.value = '2026-09-01T12:00';
picker.requestSubmit();
check(document.getElementById('custom-range-error').textContent.includes('31'), 'Oversized range has no validation message');
check(window.historyRequests === pickerRequests, 'Invalid picker input made a history request');
fromInput.value = '2026-09-04T12:00';
toInput.value = '2026-09-04T14:30';
fromInput.dispatchEvent(new Event('input', { bubbles: true }));
const draftStart = parseDateTimeInput(fromInput.value, state.timeZone).getTime();
const previousPickerZone = state.timeZone;
state.timeZone = 'utc';
controls.refreshCustomTimePicker(previousPickerZone);
check(parseDateTimeInput(fromInput.value, 'utc').getTime() === draftStart, 'Time zone change shifted the draft range');
check(document.getElementById('custom-time-zone').textContent === 'UTC', 'Picker does not show its time zone');
document.querySelector('[data-custom-preset="yesterday"]').click();
check(toInput.value.endsWith('T00:00') && fromInput.value.endsWith('T00:00'), 'Yesterday does not use calendar boundaries');
check(parseDateTimeInput(toInput.value, 'utc') - parseDateTimeInput(fromInput.value, 'utc') === 86400000, 'Yesterday is not one UTC day');
check(window.historyRequests === pickerRequests, 'A shortcut applied the draft before confirmation');
// Choose April 17 then April 19 using the shared range calendar.
const calendar = document.getElementById('custom-range-calendar');
const calendarYear = calendar.querySelector('input');
calendarYear.value = '2026';
calendarYear.dispatchEvent(new Event('change', { bubbles: true }));
const calendarMonth = calendar.querySelector('select');
calendarMonth.value = '3';
calendarMonth.dispatchEvent(new Event('change', { bubbles: true }));
check(calendar.querySelector('[data-date="2026-04-16"]').disabled, 'Calendar allows dates before retained history');
check(calendar.querySelector('[data-date="2026-04-20"]').disabled, 'Calendar allows dates after retained history');
check(!calendar.querySelector('[data-date="2026-04-18"]').disabled, 'Calendar disabled a retained date');
calendar.querySelector('[data-date="2026-04-17"]').click();
picker.requestSubmit();
check(window.historyRequests === pickerRequests, 'Calendar submitted an unfinished range');
calendar.querySelector('[data-date="2026-04-19"]').click();
check(fromInput.value === '2026-04-17T03:00', 'Calendar start is not clamped to retained history');
check(toInput.value === '2026-04-19T21:00', 'Calendar end is not clamped to retained history');
check(calendar.querySelectorAll('.in-range').length === 3, 'Calendar range highlight is incorrect');
check(window.historyRequests === pickerRequests, 'Calendar applied dates without Apply');
state.collectionIntervalMs = 250;
controls.refreshCustomTimePicker();
calendar.querySelector('[data-date="2026-04-18"]').click();
calendar.querySelector('[data-date="2026-04-18"]').click();
check(fromInput.step === '0.001' && toInput.value === '2026-04-18T23:59:59.999',
    'Sub-second collection did not preserve fractional precision');
state.collectionIntervalMs = 1000;
picker.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
check(picker.classList.contains('hidden') && document.activeElement === pickerButton, 'Escape did not close the picker and restore focus');
pickerButton.click();
for (let i = 0; picker.querySelector('button[type="submit"]').disabled && i < 100; i++) await pause(10);
check(parseDateTimeInput(fromInput.value, state.timeZone).getTime() === Math.floor(state.customFrom.getTime() / 1000) * 1000,
    'Cancel did not discard the draft');
document.body.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true }));
check(picker.classList.contains('hidden'), 'Outside pointer click did not dismiss the picker');
pickerButton.click();
for (let i = 0; picker.querySelector('button[type="submit"]').disabled && i < 100; i++) await pause(10);
document.querySelector('.time-zone-btn[data-time-zone="utc"]').click();
check(picker.classList.contains('hidden'), 'Outside settings click did not dismiss the picker');
pickerButton.click();
for (let i = 0; picker.querySelector('button[type="submit"]').disabled && i < 100; i++) await pause(10);
fromInput.value = '2026-09-04T10:00';
toInput.value = '2026-09-05T10:00';
picker.requestSubmit();
for (let i = 0; state.loadingHistory && i < 500; i++) await pause(10);
check(state.customFrom.toISOString() === '2026-09-04T10:00:00.000Z', 'Picker applied the wrong start timestamp');
check(state.customTo.toISOString() === '2026-09-05T10:00:00.000Z', 'Picker applied the wrong end timestamp');
check(window.historyRequests === pickerRequests + 1 && picker.classList.contains('hidden'), 'Apply did not fetch once and close the picker');

// A completed keyboard zoom is debounced. A newer navigation must cancel that
// pending work before it can issue a request and restore the obsolete viewport.
const zoomBaseFrom = new Date('2026-09-04T10:00:00Z');
const zoomBaseTo = new Date('2026-09-05T10:00:00Z');
const newerFrom = new Date('2026-09-04T16:00:00Z');
const newerTo = new Date('2026-09-05T04:00:00Z');
for (const scenario of [
    { name: 'Live', navigate: () => controls.goLive(), range: state.lastPresetRange || 300 },
    { name: 'preset', navigate: () => controls.setTimeRange(60), range: 60 },
    { name: 'custom range', navigate: () => controls.setCustomRange(newerFrom, newerTo), range: null },
    { name: 'local zoom', navigate: () => {
        state.currentTier = 0;
        state.currentDownsampled = false;
        check(data.tryZoomFromBuffer(newerFrom, newerTo), 'Newer local zoom was not served from the buffer');
    }, range: null, local: true },
]) {
    state.timeRange = null;
    state.customFrom = zoomBaseFrom;
    state.customTo = zoomBaseTo;
    await data.fetchCustomHistory(zoomBaseFrom, zoomBaseTo);
    await frame();
    const beforeZoom = window.historyRequests;
    cpu.canvas.dispatchEvent(new KeyboardEvent('keydown', { key: '+', bubbles: true }));
    check(state.loadingHistory && window.historyRequests === beforeZoom,
        `${scenario.name}: zoom did not enter the debounce window`);
    scenario.navigate();
    // Wait beyond the debounce and allow the replacement response to settle.
    await pause(300);
    for (let i = 0; state.loadingHistory && i < 500; i++) await pause(10);
    check(!state.loadingHistory, `${scenario.name}: replacement history did not settle`);
    check(state.timeRange === scenario.range, `${scenario.name}: pending zoom restored the obsolete viewport`);
    if (scenario.range === null) {
        check(state.customFrom.getTime() === newerFrom.getTime() && state.customTo.getTime() === newerTo.getTime(),
            `${scenario.name}: pending zoom changed the selected custom bounds`);
    }
    check(window.historyRequests === beforeZoom + (scenario.local ? 0 : 1),
        `${scenario.name}: obsolete zoom issued an extra history request`);
}

// Live device discovery must not relabel or rebuild a frozen historical view.
await data.fetchCustomHistory(state.customFrom, state.customTo);
const savedLiveSample = state.lastSample;
const historicalNet = state.selectedNet;
const historicalDisk = state.selectedDiskIo;
const historicalNetValues = state.charts.network.data.datasets[0].data.map(point => point.y);
const changedDevices = structuredClone(state.lastSample);
changedDevices.ts = new Date(Date.parse(changedDevices.ts) + 1000).toISOString();
changedDevices.net.ifaces = [{ name: 'replacement0', rx_mbps: 987, tx_mbps: 123 }];
changedDevices.disk.devices = [{ name: 'replacement-disk', read_bps: 987 }];
data.pushLiveSample(changedDevices);
check(state.selectedNet === historicalNet && state.selectedDiskIo === historicalDisk,
    'Live telemetry changed frozen historical selectors');
check(document.getElementById('net-selector').value === historicalNet,
    'Historical network selector displays the live device');
data.redrawChartsFromBuffer();
check(state.selectedNet === historicalNet &&
    JSON.stringify(state.charts.network.data.datasets[0].data.map(point => point.y)) === JSON.stringify(historicalNetValues),
    'Redrawing frozen history adopted live selectors');
document.getElementById('btn-split-network').click();
const historicalSplit = state.splitCharts.network[`net_${historicalNet}`];
check(historicalSplit, 'Historical split fixture was not created');
changedDevices.ts = new Date(Date.parse(changedDevices.ts) + 1000).toISOString();
data.pushLiveSample(changedDevices);
check(state.splitCharts.network[`net_${historicalNet}`] === historicalSplit &&
    !state.splitCharts.network.net_replacement0, 'Live discovery replaced historical split charts');
document.getElementById('btn-split-network').click();
state.lastSample = savedLiveSample;

// An older request that ignores AbortSignal cannot replace an active gesture.
const gestureFetch = window.fetch;
let releaseOldZoom, oldZoomSignal;
window.fetch = (url, options) => String(url).includes('/api/history?')
    ? new Promise(resolve => {
        oldZoomSignal = options.signal;
        releaseOldZoom = () => resolve(gestureFetch(url));
    }) : gestureFetch(url, options);
const oldZoom = data.fetchZoomedHistory(state.customFrom, state.customTo);
const gestureFrom = new Date('2026-09-03T12:00:00Z');
const gestureTo = new Date('2026-09-03T18:00:00Z');
cpu.options.scales.x.min = +gestureFrom;
cpu.options.scales.x.max = +gestureTo;
data.syncZoom({ chart: cpu, complete: false });
check(oldZoomSignal.aborted, 'Continuous gesture did not abort the old request');
releaseOldZoom();
await oldZoom;
check(+state.customFrom === +gestureFrom && +state.customTo === +gestureTo,
    'Old response restored its viewport during a continuous gesture');
window.fetch = gestureFetch;

// Exercise zoom against native one-second observations, not the dense mock
// response used by the performance fixture. Every allowed zoom retains >=12.
const zoomBaseSample = structuredClone(state.dataBuffer.find(item => item.data).data);
let keepZoomSample = () => true;
window.fetch = async (url, options) => {
    if (!String(url).includes('/api/history?')) return gestureFetch(url, options);
    const query = new URL(url, location.href).searchParams;
    const start = Date.parse(query.get('from')), end = Date.parse(query.get('to'));
    const samples = [];
    for (let ts = Math.ceil(start / 1000) * 1000; ts <= end; ts += 1000) {
        if (!keepZoomSample(ts)) continue;
        const sample = structuredClone(zoomBaseSample);
        sample.ts = new Date(ts).toISOString();
        samples.push({ ts: sample.ts, dur: 1e9, data: sample });
    }
    return new Response(JSON.stringify({ samples, tier: 0, resolution: '1s', source_resolution: '1s',
        downsampled: false, complete: true, exact_complete: true, valid_aggregations: ['data'],
        requested_from: new Date(start), requested_to: new Date(end),
        actual_from: new Date(start), actual_to: new Date(end) }), { status: 200 });
};
state.collectionIntervalMs = 1000;
state.customFrom = new Date('2026-09-05T09:59:00Z');
state.customTo = new Date('2026-09-05T10:00:00Z');
await data.fetchCustomHistory(state.customFrom, state.customTo);
await frame();
const { chartTimestamps } = await import('./js/app/chart-accessibility.js');
for (let i = 0; i < 20; i++) {
    cpu.canvas.dispatchEvent(new KeyboardEvent('keydown', { key: '+', bubbles: true }));
    await frame();
    check(state.customTo - state.customFrom >= 12000, 'Keyboard zoom exceeded the twelve-point limit');
    check(chartTimestamps(cpu).length >= 12, 'Keyboard zoom displayed fewer than twelve observations');
}
check(state.customTo - state.customFrom === 12000, 'Keyboard zoom did not reach the minimum');
const pivot = (+state.customFrom + +state.customTo) / 2;
cpu.zoomScale('x', { min: pivot - 100, max: pivot + 100 });
cpu.options.plugins.zoom.zoom.onZoomComplete({ chart: cpu });
await frame();
check(state.customTo - state.customFrom === 12000 && chartTimestamps(cpu).length >= 12,
    'Chart.js zoom bypassed the twelve-point limit');
cpu.options.scales.x.min = pivot - 100;
cpu.options.scales.x.max = pivot + 100;
data.syncZoom({ chart: cpu, complete: false });
check(state.customTo - state.customFrom === 12000, 'Continuous pinch bypassed the zoom limit');
data.syncZoom({ chart: cpu, complete: true });
await frame();

// Twelve observations on either side of an outage must all remain visible;
// an already-sparse custom range must not permit any further zoom-in.
const sparseStart = Date.parse('2026-09-05T09:59:00Z');
for (const remaining of [12, 11]) {
    keepZoomSample = ts => ts <= sparseStart + 5000 || ts >= sparseStart + (67 - remaining) * 1000;
    state.customFrom = new Date(sparseStart);
    state.customTo = new Date(sparseStart + 60000);
    await data.fetchCustomHistory(state.customFrom, state.customTo);
    await frame();
    check(chartTimestamps(cpu).length === remaining, 'Sparse zoom fixture has the wrong point count');
    cpu.options.scales.x.min = sparseStart + 55000;
    cpu.options.scales.x.max = sparseStart + 56000;
    data.syncZoom({ chart: cpu, complete: true });
    await frame();
    check(+state.customFrom === sparseStart && +state.customTo === sparseStart + 60000,
        'Sparse history zoomed past the twelve-observation limit');
    check(chartTimestamps(cpu).length === remaining, 'Sparse zoom discarded retained observations');
}
window.fetch = gestureFetch;
data.cancelHistoryRequest();

// Use real rolling ingestion over two accelerated hours, including refreshes.
// fetchHistory creates Date instances; replace the constructor as well as now.
const NativeDate = Date;
const nativeNow = Date.now;
let now = nativeNow();
window.Date = class extends NativeDate {
    constructor(...args) { super(...(args.length ? args : [now])); }
    static now() { return now; }
};
state.timeRange = 86400; state.customFrom = state.customTo = null;
await data.fetchHistory(86400);
const initialSpan = Date.parse(state.dataBuffer.at(-1).ts) - Date.parse(state.dataBuffer[0].ts);
check(initialSpan > 23 * 3600000, 'Initial history did not cover 24 hours');
let maxItems = 0;
for (let i = 0; i < 7200; i++) {
    now += 1000;
    const sample = structuredClone(state.lastSample);
    sample.ts = new Date(now).toISOString();
    data.pushLiveSample(sample);
    if (state.loadingHistory) {
        for (let waits = 0; state.loadingHistory && waits < 500; waits++) await pause(2);
        check(!state.loadingHistory, 'Rolling refresh did not settle');
    }
    maxItems = Math.max(maxItems, state.dataBuffer.length);
    check(state.dataBuffer.length <= state.maxBufferSize, 'Live buffer exceeded its limit');
}
const span = Date.parse(state.dataBuffer.at(-1).ts) - Date.parse(state.dataBuffer[0].ts);
check(span > 23 * 3600000, 'Live ingestion lost the start of the selected 24-hour interval');
check(state.historyViewEnd > now - 300000, 'Rolling snapshot stopped refreshing');
check(state.timeRange === 86400 && state.historyStatus === 'complete', 'Rolling history lost range or coverage');

// A background snapshot must not freeze live gauges while its response is pending.
const backgroundFetch = window.fetch;
let releaseBackground;
window.fetch = (url, options) => String(url).includes('/api/history?')
    ? new Promise(resolve => {
        releaseBackground = () => resolve(backgroundFetch(url, options));
    })
    : backgroundFetch(url, options);
const pendingBackground = data.fetchHistory(86400, { background: true });
check(state.loadingHistory, 'Background refresh did not start');
const liveDuringRefresh = structuredClone(state.lastSample);
liveDuringRefresh.ts = new Date(now + 1000).toISOString();
liveDuringRefresh.cpu.total.usage = 99;
state.ws.onmessage({ data: JSON.stringify(liveDuringRefresh) });
check(state.lastSample.cpu.total.usage === 99, 'Background refresh queued the live sample');
check(document.getElementById('gauge-cpu-value').textContent === '99.0%', 'Background refresh froze the CPU gauge');
check(state.liveQueue.length === 0, 'Background refresh added to the foreground live queue');
releaseBackground();
await pendingBackground;
window.fetch = backgroundFetch;

// Failure retains the previous full-span representation and reports failure.
const successfulBuffer = state.dataBuffer;
const successfulEnd = state.historyViewEnd;
const nativeFetch = window.fetch;
window.fetch = async (url, options) => String(url).includes('/api/history?')
    ? new Response('{}', {status: 503}) : nativeFetch(url, options);
now += 300000;
await data.fetchHistory(86400, { background: true });
check(state.historyStatus === 'failed', 'Failed refresh was reported complete');
check(state.dataBuffer === successfulBuffer && state.historyViewEnd === successfulEnd, 'Failed refresh discarded history');
check(document.getElementById('sampling-info').textContent.includes(i18n.t('history_failed')), 'Failed refresh is not visible');
check(document.getElementById('sampling-info').textContent.includes('Tier 2'), 'Failed refresh removed the visible tier');
window.fetch = nativeFetch;
window.Date = NativeDate;

// 5,000 observations plus explicit gaps must all survive ingestion and redraw.
const plotWidth = chartUpdates.plotWidth;
chartUpdates.plotWidth = () => 8000;
window.gapFixture = true;
state.timeRange = null; state.customFrom = new Date('2026-09-04T10:00:00Z'); state.customTo = new Date('2026-09-05T10:00:00Z');
await data.fetchCustomHistory(state.customFrom, state.customTo);
check(state.dataBuffer.filter(item => !item._gap).length === 5000, 'Wide response lost observations');
check(state.dataBuffer.some(item => item._gap), 'Wide response lost gaps');
const configuredGaps = cpu.options.plugins.kulaEnvelope.gaps;
const renderedGaps = typeof configuredGaps === 'function' ? configuredGaps() : configuredGaps;
check(state.historyGaps.length > 0 && Array.isArray(renderedGaps) &&
    renderedGaps.length === state.historyGaps.length,
    'Measurement gaps are not exposed to the neutral chart-band renderer');
check(state.dataBuffer.length <= 10000, 'Wide gap response is unbounded');
check(Date.parse(state.dataBuffer[0].ts) === state.customFrom.getTime(), 'Gap markers evicted the left edge');
const frozen = state.dataBuffer;
const frozenPoints = cpu.data.datasets[1].data.length;
for (let i = 0; i < 50; i++) {
    const sample = structuredClone(state.lastSample);
    sample.ts = new Date(Date.parse(sample.ts) + 1000).toISOString();
    data.pushLiveSample(sample);
}
check(state.dataBuffer === frozen && cpu.data.datasets[1].data.length === frozenPoints, 'Live stream modified a custom range');
chartUpdates.plotWidth = plotWidth;
window.result = { status: 'pass', charts: originalCharts.length, layout_ms: Math.round(layoutMs),
    retained_hours: span / 3600000, max_live_items: maxItems, gap_items: state.dataBuffer.length,
    data_opt_in: true, background_live_gauges: true, failure_preserves_history: true,
    horizontal_time_labels: true, visible_sampling_tier: true, custom_picker: true,
    calendar_range: true, tooltip_details_opt_in: true, crosshair_drag_safe: true,
    shaded_measurement_gaps: true, historical_device_selection: true,
    gesture_request_isolation: true, minimum_zoom_points: 12, errors };
check(errors.length === 0, errors.join('; '));
window.ready = true;
