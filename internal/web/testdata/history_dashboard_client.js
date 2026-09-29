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

// The server closes expired/revoked sessions with 1008. Return to login and
// reject an in-flight history response, then restore the selected view after
// signing in without reloading the page.
const authFetch = window.fetch;
let finishExpiredHistory;
window.fetch = (url, options) => String(url).includes('/api/history?')
    ? new Promise(resolve => { finishExpiredHistory = resolve; })
    : authFetch(url, options);
const expiredPayload = { samples: state.dataBuffer.slice(), tier: 1, resolution: '1m',
    valid_aggregations: ['data', 'min', 'max'] };
const expiredRequest = data.fetchCustomHistory(state.customFrom, state.customTo);
check(finishExpiredHistory, 'Expiry fixture did not start a history request');
const expiredSocket = state.ws;
state.csrfToken = 'expired-token';
expiredSocket.readyState = 3;
expiredSocket.onclose({ code: 1008, reason: 'session expired' });
check(!document.getElementById('login-overlay').classList.contains('hidden'),
    'Expired session did not return to login');
check(!state.ws && !state.connected && !state.reconnectTimer,
    'Expired session continued reconnecting');
check(!state.csrfToken && !state.loadingHistory && !state.dataBuffer.length && !state.liveQueue.length,
    'Expired session retained authenticated state');
finishExpiredHistory(new Response(JSON.stringify(expiredPayload)));
await expiredRequest;
check(!state.dataBuffer.length, 'A late history response restored expired session data');
window.fetch = (url, options) => String(url).endsWith('/api/login')
    ? Promise.resolve(new Response(JSON.stringify({ csrf_token: 'fresh-token' })))
    : authFetch(url, options);
document.getElementById('login-user').value = 'operator';
document.getElementById('login-pass').value = 'fixture-password';
document.getElementById('login-form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
for (let i = 0; !state.ws && i < 500; i++) await pause(10);
check(state.ws && state.ws !== expiredSocket && state.csrfToken === 'fresh-token',
    'Signing in after expiry did not open a new session');
state.ws.readyState = WebSocket.OPEN;
state.ws.onopen();
for (let i = 0; state.loadingHistory && i < 500; i++) await pause(10);
check(state.historyLoaded && state.dataBuffer.length > 0 &&
    document.getElementById('login-overlay').classList.contains('hidden'),
    'Signing in after expiry did not restore the selected history');
window.fetch = authFetch;
await frame();
const originalCharts = Object.values(Chart.instances);
const originalBuffer = state.dataBuffer;
const cpu = state.charts.cpu;

// History preparation should perform presentation work per chart, not per
// observation. Count DOM mutations rather than enforcing host-dependent time.
const replayObserver = new MutationObserver(() => {});
const replayTimings = [];
let replayMutations = 0;
for (let run = 0; run < 5; run++) {
    replayObserver.observe(document.body, { subtree: true, childList: true, attributes: true, characterData: true });
    const started = performance.now();
    data.redrawChartsFromBuffer();
    replayTimings.push(performance.now() - started);
    replayMutations = Math.max(replayMutations, replayObserver.takeRecords().length);
    replayObserver.disconnect();
    await frame();
}
check(replayMutations < originalCharts.length * 10,
    `History replay performed ${replayMutations} DOM mutations for ${originalCharts.length} charts`);
check(state.dataBuffer === originalBuffer, 'History replay replaced canonical observations');
replayTimings.sort((a, b) => a - b);
const replayPerformance = { samples: originalBuffer.length, dom_mutations: replayMutations,
    median_ms: +replayTimings[Math.floor(replayTimings.length / 2)].toFixed(2) };

// Changing a filesystem must leave every unrelated chart's data intact.
const mountSelector = document.getElementById('diskspace-selector');
const originalMount = state.selectedDiskSpace;
const otherMount = Array.from(mountSelector.options).find(option => option.value !== originalMount)?.value;
check(otherMount, 'Fixture needs two filesystems to exercise device selection');
const unrelatedDatasets = originalCharts.filter(chart => chart !== state.charts.diskspace)
    .flatMap(chart => chart.data.datasets.map(dataset => ({ dataset, points: dataset.data })));
const originalGaps = state.historyGaps;
mountSelector.value = otherMount;
mountSelector.dispatchEvent(new Event('change', { bubbles: true }));
check(unrelatedDatasets.every(({ dataset, points }) => dataset.data === points),
    'Changing filesystem replayed unrelated chart data');
check(state.historyGaps === originalGaps, 'Changing filesystem replaced shared gap metadata');
const selectedFS = originalBuffer.at(-1).data.disk.filesystems.find(fs => fs.mount === otherMount);
check(state.charts.diskspace.data.datasets[0].data.at(-1).y === selectedFS.used_pct,
    'Filesystem selection did not update the selected chart');
mountSelector.value = originalMount;
mountSelector.dispatchEvent(new Event('change', { bubbles: true }));
await frame();

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

// Rebuilding split cards must not leave document listeners holding removed cards.
const addDocumentListener = document.addEventListener;
const removeDocumentListener = document.removeEventListener;
let splitClickListeners = 0;
document.addEventListener = function(type, ...args) {
    if (type === 'click') splitClickListeners++;
    return addDocumentListener.call(this, type, ...args);
};
document.removeEventListener = function(type, ...args) {
    if (type === 'click') splitClickListeners--;
    return removeDocumentListener.call(this, type, ...args);
};
try {
    for (let cycle = 0; cycle < 3; cycle++) {
        document.getElementById('btn-split-network').click();
        const card = document.querySelector('[data-split-type="network"]:has(.chart-settings-dropdown)');
        card.querySelector('button[title="Graph Bounds"]').click();
        check(!card.querySelector('.chart-settings-dropdown').classList.contains('hidden'),
            'Split graph bounds did not open');
        document.getElementById('btn-theme').click();
        check(card.querySelector('.chart-settings-dropdown').classList.contains('hidden'),
            'Unrelated icon click did not close split graph bounds');
        card.querySelector('button[title="Graph Bounds"]').click();
        document.body.click();
        check(card.querySelector('.chart-settings-dropdown').classList.contains('hidden'),
            'Outside click did not close split graph bounds');
        document.getElementById('btn-split-network').click();
        await frame();
    }
} finally {
    document.addEventListener = addDocumentListener;
    document.removeEventListener = removeDocumentListener;
}
check(splitClickListeners === 0, 'Split rebuild retained document click listeners');
check(Object.values(Chart.instances).length === originalCharts.length, 'Split rebuild retained chart instances');

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
check(document.getElementById('sampling-info').textContent.includes('Tier 0'),
    'Tier 0 was displayed with a different file index');
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
check(document.querySelector('[data-i18n="charts"]').textContent === i18n.t('charts') &&
    i18n.t('charts') !== 'Charts' && i18n.t('selected_duration') !== 'Selected duration',
    'Chart personalization or custom range text is not translated');
check(document.getElementById('btn-apply-custom').textContent === i18n.t('apply_range') &&
    document.getElementById('btn-cancel-custom').textContent === i18n.t('cancel'),
    'Custom range button text is not translated');
check(document.getElementById('sys-info').textContent.includes(i18n.t('source')) &&
    document.getElementById('sys-info').textContent.includes(i18n.t('self')),
    'Footer status text is not translated');
check([...document.querySelectorAll('.btn-chart-image')].every(button =>
    button.title === 'Zapisz jako obraz' && button.getAttribute('aria-label') === button.title),
    'Save-as-image buttons were not retitled by a language change');
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

// Every chart card offers a PNG snapshot before its expand control. The
// export is opaque (cards are translucent), keeps the canvas resolution, adds
// the header/footer rows and downloads under a descriptive name.
const imageButtons = document.querySelectorAll('.chart-card .btn-chart-image');
check(imageButtons.length > 0 &&
    imageButtons.length === document.querySelectorAll('.chart-card .btn-expand-chart').length,
    'Save-as-image button is missing from some chart cards');
const cpuImageButton = cpu.canvas.closest('.chart-card').querySelector('.btn-chart-image');
check(cpuImageButton.nextElementSibling?.classList.contains('btn-expand-chart'),
    'Save-as-image button is not placed before the expand button');
const nativeCreateObjectURL = URL.createObjectURL;
const nativeAnchorClick = HTMLAnchorElement.prototype.click;
let imageDownload = null;
URL.createObjectURL = blob => { imageDownload = { blob }; return nativeCreateObjectURL.call(URL, blob); };
HTMLAnchorElement.prototype.click = function () { if (imageDownload) imageDownload.filename = this.download; };
try {
    cpuImageButton.click();
    for (let i = 0; i < 300 && !imageDownload?.filename; i++) await pause(10);
} finally {
    URL.createObjectURL = nativeCreateObjectURL;
    HTMLAnchorElement.prototype.click = nativeAnchorClick;
}
check(imageDownload?.blob?.type === 'image/png', 'Chart image was not exported as PNG');
check(/^kula-(?:[\p{L}\p{N}-]+-)?cpu-usage-\d{8}-\d{6}Z?\.png$/u.test(imageDownload.filename),
    `Unexpected chart image name ${imageDownload.filename}`);
const snapshot = await createImageBitmap(imageDownload.blob);
check(snapshot.width === cpu.canvas.width + 32, 'Chart image width does not match the chart canvas');
check(snapshot.height > cpu.canvas.height + 40, 'Chart image omitted the title and footer rows');
const snapshotPixels = new OffscreenCanvas(snapshot.width, snapshot.height).getContext('2d');
snapshotPixels.drawImage(snapshot, 0, 0);
check(snapshotPixels.getImageData(1, 1, 1, 1).data[3] === 255, 'Chart image background is transparent');
check(!cpuImageButton.disabled, 'Save-as-image button stayed disabled after export');

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
check(document.getElementById('sampling-info').textContent.includes('Tier 1'), 'Sampling info lost its tier after loading');
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
const calendar = document.getElementById('custom-range-calendar');
const clock = document.getElementById('custom-range-clock');
const dial = clock.querySelector('.range-dial');
const wheel = unit => clock.querySelector(`.range-wheel[data-unit="${unit}"]`);
const wheelValues = () => [...clock.querySelectorAll('.range-wheel')].map(node => node.getAttribute('aria-valuetext')).join();
// A dial press at an angle (clockwise from 12) and a radius in dial units.
const pressDial = (angle, radius = 78) => {
    const box = dial.getBoundingClientRect();
    const distance = box.width / 2 * radius / 100;
    const at = {
        clientX: box.left + box.width / 2 + distance * Math.sin(angle * Math.PI / 180),
        clientY: box.top + box.height / 2 - distance * Math.cos(angle * Math.PI / 180),
        pointerId: 1, button: 0, bubbles: true,
    };
    dial.dispatchEvent(new PointerEvent('pointerdown', at));
    dial.dispatchEvent(new PointerEvent('pointerup', at));
};
const stepWheel = (unit, step) => {
    const button = wheel(unit).parentElement.querySelector(`[data-step="${step}"]`);
    button.dispatchEvent(new PointerEvent('pointerdown', { button: 0, bubbles: true }));
    button.dispatchEvent(new PointerEvent('pointerup', { button: 0, bubbles: true }));
};
const submitButton = picker.querySelector('button[type="submit"]');
const rangeError = document.getElementById('custom-range-error');
const fromDate = document.getElementById('custom-from-date');
const toDate = document.getElementById('custom-to-date');
const fromTime = document.getElementById('custom-from-time');
const toTime = document.getElementById('custom-to-time');
const { parseDateTimeInput } = await import('./js/app/format.js');
const draftFrom = () => `${fromDate.dataset.day}T${fromTime.dataset.time}`;
const draftTo = () => `${toDate.dataset.day}T${toTime.dataset.time}`;
const typeTime = (input, value) => {
    input.value = value;
    input.dispatchEvent(new Event('input', { bubbles: true }));
};
const showMonth = value => {
    const select = calendar.querySelector('.range-calendar-month');
    select.value = value;
    select.dispatchEvent(new Event('change', { bubbles: true }));
    check(select.value === value, `Calendar cannot show ${value}`);
};
const clickDay = day => calendar.querySelector(`[data-date="${day}"]`).click();
const openPicker = async () => {
    pickerButton.click();
    for (let i = 0; submitButton.disabled && i < 100; i++) await pause(10);
};
await openPicker();
check(pickerButton.getAttribute('aria-expanded') === 'true', 'Picker does not announce its open state');
check(parseDateTimeInput(draftFrom(), state.timeZone).getTime() === Math.floor(state.customFrom.getTime() / 1000) * 1000,
    `Picker discarded the selected range: ${draftFrom()}`);
check(!fromTime.dataset.time.includes('.') && !/\d[.,]\d{3}/.test(fromTime.value),
    'One-second collection exposes fractional precision');
check(calendar.querySelectorAll('.range-calendar-grid').length === 1 &&
    !picker.querySelector('input[type="date"], input[type="datetime-local"]'), 'Picker shows more than one calendar');
check(document.activeElement.dataset.date === fromDate.dataset.day, 'Opening the picker did not focus the start day');
const pickerRequests = window.historyRequests;
// Presets and the current view can produce drafts the calendar cannot, so
// validation still covers them. Write such drafts to the fields directly.
fromDate.dataset.day = '2026-04-21';
typeTime(fromTime, '12:00:00');
toDate.dataset.day = '2026-09-02';
typeTime(toTime, '12:00:00');
check(rangeError.textContent === i18n.t('range_max_31_days'), 'Oversized range validation lost precedence');
toDate.dataset.day = '2026-04-22';
typeTime(toTime, '12:00:00');
check(rangeError.textContent === i18n.t('range_outside_retention') && submitButton.disabled,
    'A range without retained days was accepted');
picker.requestSubmit();
check(window.historyRequests === pickerRequests, 'Invalid picker input made a history request');
// A calendar day spans the whole day. Typed times survive later day changes.
document.querySelector('[data-custom-preset="1h"]').click();
showMonth('2026-09');
clickDay('2026-09-04');
clickDay('2026-09-04');
check(draftFrom() === '2026-09-04T00:00:00' && draftTo() === '2026-09-04T23:59:59',
    `A calendar day does not span the whole day: ${draftFrom()} - ${draftTo()}`);
check(calendar.querySelectorAll('.in-range').length === 1, 'Single-day highlight is incorrect');
typeTime(fromTime, '14:30:00');
typeTime(toTime, '12:00:00');
picker.requestSubmit();
check(rangeError.textContent === i18n.t('range_start_before_end') && submitButton.disabled,
    'Reversed range has no validation message');
check(window.historyRequests === pickerRequests, 'Invalid picker input made a history request');
clickDay('2026-09-02');
clickDay('2026-09-05');
check(draftFrom() === '2026-09-02T14:30:00' && draftTo() === '2026-09-05T12:00:00',
    `Calendar clicks discarded typed times: ${draftFrom()} - ${draftTo()}`);
check(!submitButton.disabled, 'A valid typed range was rejected');
// The From and To dates choose which end the next click sets.
toDate.click();
check(toDate.getAttribute('aria-pressed') === 'true' && document.activeElement.dataset.date === '2026-09-05',
    'The To date does not point the calendar at the end');
clickDay('2026-09-07');
check(draftFrom() === '2026-09-02T14:30:00' && draftTo() === '2026-09-07T12:00:00', 'Editing the end changed the start');
// Time fields use the UI language's clock rather than the browser's, accept
// loose typing and step the part at the caret with the arrow keys.
check(/^02:30:00\sPM$/u.test(fromTime.value) && /^12:00:00\sPM$/u.test(toTime.value),
    `English times are not shown on a 12-hour clock: ${fromTime.value} - ${toTime.value}`);
await i18n.loadTranslations('pl');
i18n.applyTranslations();
document.dispatchEvent(new Event('kula-i18n-changed'));
check(fromTime.value === '14:30:00' && toTime.value === '12:00:00' && fromTime.placeholder === 'hh:mm:ss',
    `Polish times are not shown on a 24-hour clock: ${fromTime.value} - ${toTime.value}`);
typeTime(fromTime, '930');
check(draftFrom() === '2026-09-02T09:30:00', `Compact typing was not understood: ${draftFrom()}`);
fromTime.dispatchEvent(new Event('change', { bubbles: true }));
check(fromTime.value === '09:30:00', `Leaving the field did not tidy the time: ${fromTime.value}`);
fromTime.focus();
fromTime.setSelectionRange(4, 4);
fromTime.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true, cancelable: true }));
check(draftFrom() === '2026-09-02T09:29:00' && fromTime.value === '09:29:00' &&
    fromTime.selectionStart === 3 && fromTime.selectionEnd === 5, 'ArrowDown did not step the minutes');
check(wheelValues() === '09,29,00' && clock.querySelectorAll('.range-dial-label.inner').length === 12,
    `The Polish clock does not follow the time field on a 24-hour clock: ${wheelValues()}`);
pressDial(60, 52);
check(draftFrom() === '2026-09-02T14:00:00' && fromTime.value === '14:00:00',
    `The inner ring does not choose afternoon hours: ${draftFrom()}`);
typeTime(fromTime, '25:00');
check(rangeError.textContent.startsWith(i18n.t('range_time_invalid')) && submitButton.disabled &&
    document.getElementById('custom-from-field').classList.contains('invalid') &&
    !document.getElementById('custom-to-field').classList.contains('invalid'), 'An impossible time was accepted');
typeTime(fromTime, '14:30');
fromTime.blur();
await i18n.loadTranslations('en');
i18n.applyTranslations();
document.dispatchEvent(new Event('kula-i18n-changed'));
check(/^02:30:00\sPM$/u.test(fromTime.value) && draftFrom() === '2026-09-02T14:30:00',
    `Switching back to English lost the 12-hour clock: ${fromTime.value}`);
// A focused time field swaps the calendar for that end's clock, a dial and
// wheels in step with the field. The dial sets its unit and zeroes the finer
// ones, then moves from the hours to the minutes.
toTime.focus();
check(calendar.classList.contains('pane-hidden') && !clock.classList.contains('pane-hidden') &&
    document.getElementById('custom-to-field').classList.contains('active') &&
    toDate.getAttribute('aria-pressed') === 'false', 'Focusing a time did not show its clock');
check(wheelValues() === '12,00,00,PM' && dial.getAttribute('aria-label') === i18n.t('hour'),
    `The English clock is not a 12-hour clock: ${wheelValues()}`);
pressDial(180);
check(draftTo() === '2026-09-07T18:00:00' && /^06:00:00\sPM$/u.test(toTime.value) &&
    dial.getAttribute('aria-label') === i18n.t('minute'), `The dial did not set the hour: ${draftTo()}`);
pressDial(90);
check(draftTo() === '2026-09-07T18:15:00' && wheelValues() === '6,15,00,PM', `The dial did not set the minutes: ${draftTo()}`);
// A wheel changes only its own unit: pick a row, press + or −, or use the arrow keys.
wheel('minute').querySelector('[data-index="45"]').click();
check(draftTo() === '2026-09-07T18:45:00', `A wheel row did not set the minutes: ${draftTo()}`);
stepWheel('second', 1);
check(draftTo() === '2026-09-07T18:45:01' && document.activeElement === wheel('second'), `+ did not add a second: ${draftTo()}`);
stepWheel('second', -1);
stepWheel('second', -1);
check(draftTo() === '2026-09-07T18:45:59', `− did not wrap within the seconds: ${draftTo()}`);
wheel('hour').focus();
wheel('hour').dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true }));
wheel('period').dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
check(draftTo() === '2026-09-07T07:45:59' && dial.getAttribute('aria-label') === i18n.t('hour'),
    `Wheel keys did not step the hour and day period: ${draftTo()}`);
check(!submitButton.disabled, 'A clock-picked range was rejected');
clock.querySelector('[data-shortcut="end_of_day"]').click();
check(draftTo() === '2026-09-07T23:59:59', `End of day did not end the day: ${draftTo()}`);
clock.querySelector('[data-shortcut="now"]').click();
check(Math.abs(parseDateTimeInput(draftTo(), state.timeZone) - Date.now()) < 5000, `Now did not end the range now: ${draftTo()}`);
fromTime.focus();
check(clock.querySelector('.range-clock-title').textContent.startsWith(i18n.t('from')) &&
    clock.querySelector('[data-shortcut="start_of_day"]') && !clock.querySelector('[data-shortcut="now"]') &&
    wheelValues() === '2,30,00,PM', `The start clock shows the wrong end: ${wheelValues()}`);
clock.querySelector('.range-clock-back').click();
check(!calendar.classList.contains('pane-hidden') && clock.classList.contains('pane-hidden') &&
    fromDate.getAttribute('aria-pressed') === 'true' && document.activeElement.dataset.date === '2026-09-02',
    'The clock does not return to the calendar at its end');
// Beyond 31 days, a click starts a new range rather than producing an error.
fromDate.click();
showMonth('2026-04');
clickDay('2026-04-18');
showMonth('2026-09');
clickDay('2026-09-03');
check(fromDate.dataset.day === '2026-09-03' && toDate.dataset.day === '2026-09-03' &&
    calendar.querySelector('.range-calendar-hint').textContent === i18n.t('pick_end_day'),
    'A span beyond 31 days did not restart the range');
clickDay('2026-09-04');
const draftStart = parseDateTimeInput(draftFrom(), state.timeZone).getTime();
const previousPickerZone = state.timeZone;
state.timeZone = 'utc';
controls.refreshCustomTimePicker(previousPickerZone);
check(parseDateTimeInput(draftFrom(), 'utc').getTime() === draftStart, 'Time zone change shifted the draft range');
check(document.getElementById('custom-time-zone').textContent === 'UTC', 'Picker does not show its time zone');
document.querySelector('[data-custom-preset="yesterday"]').click();
check(fromDate.dataset.day === toDate.dataset.day && fromTime.dataset.time === '00:00:00' &&
    toTime.dataset.time === '23:59:59',
    'Yesterday does not cover one calendar day');
check(parseDateTimeInput(draftTo(), 'utc') - parseDateTimeInput(draftFrom(), 'utc') === 86399000, 'Yesterday is not one UTC day');
check(window.historyRequests === pickerRequests, 'A shortcut applied the draft before confirmation');
// Retention limits the days, not the times: the first and last retained days
// run from midnight to midnight although history starts at 03:00 and ends at 21:00.
showMonth('2026-04');
check(calendar.querySelector('[data-date="2026-04-16"]').getAttribute('aria-disabled') === 'true',
    'Calendar allows dates before retained history');
check(calendar.querySelector('[data-date="2026-04-20"]').getAttribute('aria-disabled') === 'true',
    'Calendar allows dates after retained history');
check(!calendar.querySelector('[data-date="2026-04-18"]').hasAttribute('aria-disabled'), 'Calendar disabled a retained date');
check(calendar.querySelector('.calendar-prev').disabled, 'Calendar navigates before retained history');
clickDay('2026-04-17');
clickDay('2026-04-19');
check(draftFrom() === '2026-04-17T00:00:00', `Calendar start was clamped to retained history: ${draftFrom()}`);
check(draftTo() === '2026-04-19T23:59:59', `Calendar end was clamped to retained history: ${draftTo()}`);
check(!submitButton.disabled, 'Times outside retained history made the range invalid');
check(calendar.querySelectorAll('.in-range').length === 3, 'Calendar range highlight is incorrect');
check(window.historyRequests === pickerRequests, 'Calendar applied dates without Apply');
clickDay('2026-04-21');
check(fromDate.dataset.day === '2026-04-17' && toDate.dataset.day === '2026-04-19', 'A day without history was selectable');
calendar.querySelector('[data-date="2026-04-19"]').focus();
document.activeElement.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }));
check(document.activeElement.dataset.date === '2026-04-20', 'Arrow keys cannot cross days without history');
document.activeElement.dispatchEvent(new KeyboardEvent('keydown', { key: 'PageDown', bubbles: true }));
check(document.activeElement.dataset.date === '2026-05-20' &&
    calendar.querySelector('.range-calendar-month').value === '2026-05', 'PageDown did not move to the next month');
state.collectionIntervalMs = 250;
controls.refreshCustomTimePicker();
showMonth('2026-04');
clickDay('2026-04-18');
clickDay('2026-04-18');
check(draftTo() === '2026-04-18T23:59:59.999' && /59[.,]999/.test(toTime.value),
    'Sub-second collection did not preserve fractional precision');
state.collectionIntervalMs = 1000;
picker.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
check(picker.classList.contains('hidden') && document.activeElement === pickerButton, 'Escape did not close the picker and restore focus');
await openPicker();
check(parseDateTimeInput(draftFrom(), state.timeZone).getTime() === Math.floor(state.customFrom.getTime() / 1000) * 1000,
    'Cancel did not discard the draft');
document.body.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true }));
check(picker.classList.contains('hidden'), 'Outside pointer click did not dismiss the picker');
await openPicker();
document.querySelector('.time-zone-btn[data-time-zone="utc"]').click();
check(picker.classList.contains('hidden'), 'Outside settings click did not dismiss the picker');
await openPicker();
// Today's end defaults to now, so the chart does not end in empty future time.
const today = new Date().toISOString().slice(0, 10);
showMonth(today.slice(0, 7));
clickDay(today);
check(Math.abs(parseDateTimeInput(draftTo(), 'utc') - Date.now()) < 5000, `Today does not end now: ${draftTo()}`);
fromDate.click();
showMonth('2026-09');
clickDay('2026-09-04');
clickDay('2026-09-05');
typeTime(fromTime, '10:00:00');
typeTime(toTime, '10:00:00');
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
check(document.getElementById('sampling-info').textContent.includes('Tier 1'), 'Failed refresh removed the visible tier');
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

// Physical identities survive kernel-name swaps in selectors, normal charts,
// split charts and extrema. Legacy samples must produce a gap for a stable ID.
const gapItemCount = state.dataBuffer.length;
const { diskDOMKey } = await import('./js/app/disk-identity.js');
const splitModule = await import('./js/app/split.js');
const diskSample = (offset, devices) => {
    const sample = structuredClone(state.lastSample);
    sample.ts = new Date(Date.parse(sample.ts) + offset * 1000).toISOString();
    sample.disk.devices = devices.map(d => ({ reads_ps: 1, writes_ps: 2, write_bps: 3, temp: 40, ...d }));
    return sample;
};
const diskBefore = diskSample(0, [{ id: 'wwid:A', name: 'sda', read_bps: 10 }, { id: 'wwid:B', name: 'sdb', read_bps: 100 }]);
const diskAfter = diskSample(1, [{ id: 'wwid:B', name: 'sda', read_bps: 200 }, { id: 'wwid:A', name: 'sdb', read_bps: 20 }]);
const diskLegacy = diskSample(-1, [{ name: 'sda', read_bps: 999 }]);
state.selectedDiskIo = 'sda'; state.selectedDiskTemp = 'sda';
data.updateSelectors(diskBefore);
check(state.selectedDiskIo === 'wwid:A' && state.selectedDiskTemp === 'wwid:A', 'Old disk selection was not migrated');
state.lastSample = diskAfter;
data.updateSelectors(diskAfter);
const diskSelector = document.getElementById('diskio-selector');
check(diskSelector.value === 'wwid:A' && diskSelector.selectedOptions[0].textContent === 'sdb', 'Disk rename changed selection or left stale label');
check(diskSelector.selectedOptions[0].title === 'wwid:A', 'Stable ID missing from disk details');
const minimumDisks = structuredClone(diskAfter), maximumDisks = structuredClone(diskAfter);
minimumDisks.disk.devices.reverse();
minimumDisks.disk.devices.find(d => d.id === 'wwid:A').read_bps = 5;
maximumDisks.disk.devices.find(d => d.id === 'wwid:A').read_bps = 25;
state.dataBuffer = [diskLegacy, diskBefore, {
    ts: diskAfter.ts, data: diskAfter, min: minimumDisks, max: maximumDisks,
}];
state.currentAggregation = 'avg'; state.validAggregations = ['data', 'min', 'max'];
data.redrawChartsFromBuffer();
let diskPoints = state.charts.diskio.data.datasets[0].data;
check(diskPoints.length === 3 && diskPoints[0].y === null && diskPoints[1].y === 10 && diskPoints[2].y === 20,
    'Disk chart joined legacy/name-swapped readings or filled a false zero');
check(state.charts.diskio.data.datasets[0].$kulaEnvelope.at(-2) === 5 &&
    state.charts.diskio.data.datasets[0].$kulaEnvelope.at(-1) === 25, 'Disk envelopes followed array order or kernel name');
splitModule.applySplitFromConfig({ disk_io: true, disk_temp: true });
data.updateSelectors(diskBefore);
const physicalChart = state.splitCharts.diskio['diskio_wwid:A'];
data.updateSelectors(diskAfter);
check(state.splitCharts.diskio['diskio_wwid:A'] === physicalChart, 'Kernel rename replaced the physical disk chart');
data.redrawChartsFromBuffer();
diskPoints = physicalChart.data.datasets[0].data;
check(diskPoints.at(-1).y === 20, 'Split disk chart followed kernel name');
check(physicalChart.data.datasets[0].$kulaEnvelope.at(-2) === 5 &&
    physicalChart.data.datasets[0].$kulaEnvelope.at(-1) === 25, 'Split disk envelopes mixed physical drives');
check(document.getElementById(`card-split-diskio-${diskDOMKey('wwid:A')}`).querySelector('h3').textContent.endsWith('sdb'),
    'Split disk title did not follow current kernel name');
check(state.splitCharts.disktemp['disktemp_wwid:A'].data.datasets[0].data.at(-1).y === 40,
    'Split temperature did not follow physical disk');
const diskReplacement = diskSample(2, [{ id: 'wwid:C', name: 'sdb', read_bps: 9999 }]);
data.updateSelectors(diskReplacement);
check(state.selectedDiskIo === 'wwid:A', 'Replacement drive inherited an unavailable disk selection');
await frame();
for (const chart of Object.values(Chart.instances)) {
    check(chart.options.parsing === false, 'A chart re-enabled data parsing');
    for (const dataset of chart.data.datasets) {
        check(dataset.data.every(point => Number.isFinite(point.x) &&
            (point.y === null || Number.isFinite(point.y))), 'Unparsed chart data contains an invalid coordinate');
    }
}
// Exercise the additive compatibility contract through the real fetch,
// aggregation buttons, rendering, split charts, notices and export paths.
const compatibilityFetch = window.fetch;
const gibibyte = 1024 ** 3;
window.fetch = async (url, options) => {
    const response = await compatibilityFetch(url, options);
    if (!String(url).includes('/api/history?')) return response;
    const payload = await response.json();
    payload.valid_aggregations = ['data'];
    payload.available_aggregations = ['data', 'min', 'max'];
    payload.extrema_profiles = { legacy: ['cpu.total.usage', 'cpu.total.user', 'mem.used'], current: ['*'] };
    payload.samples.forEach((item, index) => {
        item.extrema_profile = index < payload.samples.length / 2 ? 'legacy' : 'current';
        item.data.net.tcp.curr_estab = 3;
        item.min.net.tcp.curr_estab = 2;
        item.max.net.tcp.curr_estab = item.extrema_profile === 'legacy' ? 2 : 4;
        for (const [block, used] of [[item.data, 20], [item.min, 10], [item.max, 80]]) {
            const filesystem = block.disk.filesystems.find(fs => fs.mount === '/');
            Object.assign(filesystem, { used: used * gibibyte, total: 100 * gibibyte, used_pct: used });
            block.apps.mysql = { replica_io_running: true, replica_sql_running: false,
                replica_seconds_behind: -1, replica_count: 0 };
        }
    });
    return new Response(JSON.stringify(payload), { status: 200, headers: { 'Content-Type': 'application/json' } });
};
state.timeRange = null;
state.customFrom = new Date('2026-09-05T07:00:00Z');
state.customTo = new Date('2026-09-05T10:00:00Z');
await data.fetchCustomHistory(state.customFrom, state.customTo);
document.querySelector('#agg-presets-list [data-agg="max"]').click();
await frame();
check(!document.getElementById('agg-presets-list').classList.contains('hidden'), 'Legacy compatibility hid aggregation choices');
const legacyCPU = state.charts.cpu.data.datasets[4].data.find(point => point.extremaSource === 'legacy');
check(legacyCPU && Number.isFinite(legacyCPU.y), 'Legacy CPU maximum was unavailable');
const connectionPoints = state.charts.connections.data.datasets[3].data;
check(connectionPoints.some(point => point.extremaSource === 'unavailable' && point.y === null), 'Copied legacy TCP extrema were plotted');
check(connectionPoints.some(point => point.extremaSource === 'current' && point.y === 4), 'Current TCP extrema were hidden by older buckets');
state.charts.connections.canvas.closest('.chart-card').scrollIntoView({ behavior: 'instant', block: 'center' });
for (let attempt = 0; attempt < 100 && !document.querySelector('#card-connections .chart-aggregation-note'); attempt++) {
    await frame();
}
check(document.querySelector('#card-connections .chart-aggregation-note')?.textContent === i18n.t('history_extrema_unavailable'),
    'Unavailable series have no chart explanation');
const compatibilityCSV = (await import('./js/app/chart-accessibility.js')).chartCSV(state.charts.connections);
check(compatibilityCSV.includes('unavailable') && compatibilityCSV.includes('current'), 'CSV lost per-interval extrema availability');
if (!state.splitNet) document.getElementById('btn-split-network').click();
data.redrawChartsFromBuffer();
const splitPoints = state.splitCharts.network.net_eth0.data.datasets[0].data;
check(splitPoints.some(point => point.extremaSource === 'unavailable' && point.y === null), 'Split chart plotted unsupported legacy extrema');
check(splitPoints.some(point => point.extremaSource === 'current' && Number.isFinite(point.y)), 'Split chart lost current extrema');

// Unavailable replication lag remains a gap in every aggregation. Filesystem
// tooltips use the same selected block as the percentage, including split cards.
const { formatBytesShort } = await import('./js/app/format.js');
state.selectedDiskSpace = '/';
for (const split of [false, true]) {
    splitModule.applySplitFromConfig({ disk_space: split });
    for (const [aggregation, used] of [['avg', 20], ['min', 10], ['max', 80]]) {
        document.querySelector(`#agg-presets-list [data-agg="${aggregation}"]`).click();
        const lag = state.charts.mysqlRepl.data.datasets[0];
        check(lag.data.length > 0 && lag.data.every(point => point.y === null),
            `${aggregation} plotted unavailable MySQL lag`);
        check(!lag.$kulaEnvelope, `${aggregation} retained unavailable MySQL lag extrema`);
        const chart = split ? state.splitCharts.diskspace['diskspace_/'] : state.charts.diskspace;
        const dataset = chart.data.datasets[0];
        const point = dataset.data.find(point => point.extremaSource === 'current');
        check(point?.y === used && point.used === used * gibibyte && point.total === 100 * gibibyte,
            `${aggregation} filesystem tooltip metadata differs from the selected block (split=${split})`);
        const label = chart.options.plugins.tooltip.callbacks.label({ raw: point,
            parsed: { y: point.y }, dataset });
        check(label.includes(`${used.toFixed(1)}%`) &&
            label.includes(`${formatBytesShort(used * gibibyte)} / ${formatBytesShort(100 * gibibyte)}`),
            `${aggregation} filesystem tooltip mixed representative and extrema values (split=${split})`);
    }
}
splitModule.applySplitFromConfig({ disk_space: false });
document.querySelector('#agg-presets-list [data-agg="avg"]').click();
await frame();
check(state.charts.connections.data.datasets[3].data.filter(point => !point.extremaUnavailable).some(point => point.y === 3),
    'Avg did not restore representative history');

// A short preset can still be answered in buckets: an unaligned five-minute
// window spans 301 one-second buckets, so a 300-point request returns 2s
// buckets with extrema even though the point budget suggests native data.
// Live samples must not extend that view (without extrema they rendered as a
// growing gap in Max), and a data-only response must suspend, not discard,
// the Max choice.
let bucketedView = true;
window.fetch = async (url, options) => {
    const response = await compatibilityFetch(url, options);
    if (!String(url).includes('/api/history?')) return response;
    const payload = await response.json();
    payload.samples = payload.samples.filter((_, index) => index % 3 === 0);
    Object.assign(payload, { tier: 0, source_resolution: '1s', complete: true, exact_complete: true },
        bucketedView
            ? { resolution: '2s', downsampled: true, valid_aggregations: ['data', 'min', 'max'],
                available_aggregations: ['data', 'min', 'max'] }
            : { resolution: '1s', downsampled: false, valid_aggregations: ['data'],
                available_aggregations: ['data'] });
    return new Response(JSON.stringify(payload), { status: 200, headers: { 'Content-Type': 'application/json' } });
};
let liveNow = Math.max(NativeDate.now(), Date.parse(state.lastSample?.ts) || 0) + 60000;
window.Date = class extends NativeDate {
    constructor(...args) { super(...(args.length ? args : [liveNow])); }
    static now() { return liveNow; }
};
const pushLive = async () => {
    liveNow += 1000;
    const sample = structuredClone(state.lastSample);
    sample.ts = new Date(liveNow).toISOString();
    data.pushLiveSample(sample);
    for (let waits = 0; state.loadingHistory && waits < 500; waits++) await pause(2);
};
state.timeRange = 300; state.customFrom = state.customTo = null; state.historyViewEnd = null;
await data.fetchHistory(300);
document.querySelector('#agg-presets-list [data-agg="max"]').click();
check(state.currentAggregation === 'max' && state.historyViewEnd !== null,
    'A bucketed short preset was treated as a live-streamable view');
const bucketedRequests = window.historyRequests;
for (let i = 0; i < 5; i++) await pushLive();
check(window.historyRequests > bucketedRequests, 'A bucketed short preset did not refresh its snapshot');
const liveTotal = state.charts.cpu.data.datasets[4];
check(state.charts.cpu.data.datasets.every(dataset => dataset.data.every(point => !point.extremaUnavailable)),
    'Live samples without extrema were appended to a Max view');
check(Number.isFinite(liveTotal.data.at(-1).y) && liveNow - liveTotal.data.at(-1).x <= 3000,
    'A bucketed short preset stopped advancing in Max');

bucketedView = false;
await data.fetchHistory(300);
check(state.currentAggregation === 'avg' && state.suspendedAggregation === 'max' && state.historyViewEnd === null,
    'A data-only native preset did not fall back to streaming Avg');
await pushLive();
check(liveTotal.data.at(-1).x === liveNow && Number.isFinite(liveTotal.data.at(-1).y),
    'A native raw preset stopped streaming live samples');
bucketedView = true;
await data.fetchHistory(300);
check(state.currentAggregation === 'max' && state.suspendedAggregation === null,
    'A data-only response discarded the Max selection');
check(new URL(location.href).searchParams.get('agg') === 'max', 'Restored Max was not written back to the share URL');
document.querySelector('#agg-presets-list [data-agg="avg"]').click();
window.Date = NativeDate;
window.fetch = compatibilityFetch;
await (await import('./audit-system-info.js')).testSystemInfo();
window.result = { status: 'pass', charts: originalCharts.length, layout_ms: Math.round(layoutMs), system_info: true,
    session_expiry_relogin: true,
    history_replay: replayPerformance, scoped_device_replay: true, numeric_chart_points: true,
    retained_hours: span / 3600000, max_live_items: maxItems, gap_items: gapItemCount,
    data_opt_in: true, background_live_gauges: true, failure_preserves_history: true,
    horizontal_time_labels: true, visible_sampling_tier: true, custom_picker: true,
    calendar_range: true, tooltip_details_opt_in: true, crosshair_drag_safe: true,
    shaded_measurement_gaps: true, historical_device_selection: true, stable_disk_identity: true,
    gesture_request_isolation: true, minimum_zoom_points: 12, legacy_metric_compatibility: true,
    unavailable_replication_lag: true, filesystem_aggregation_tooltips: true,
    bucketed_live_preset: true, suspended_aggregation: true, chart_image_export: true, errors };
check(errors.length === 0, errors.join('; '));
window.ready = true;
