// Run with: node internal/web/testdata/live_dashboard_test.mjs
// Starts the real `kula serve` with the shipped config.example.yaml (storage,
// address and port overridden through KULA_* variables) and drives the real
// dashboard over live WebSocket data in Chromium. Set KULA_BINARY to reuse a
// built binary; otherwise the test builds one with the Go toolchain.
import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { delay, findChromium, launchChromium, stopProcess } from './chromium_test_helper.mjs';

const root = fileURLToPath(new URL('../../../', import.meta.url));
const browserPath = findChromium();
if (!browserPath) throw new Error('Chromium/Chrome not found; set KULA_CHROMIUM');
if (typeof WebSocket !== 'function') throw new Error('Node.js 22+ is required');

const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'kula-live-test-'));
let server, serverClosed, browser, browserClosed, socket;
let serverOutput = '';
const errors = [];

async function freePort() {
    return new Promise((resolve, reject) => {
        const probe = net.createServer();
        probe.once('error', reject);
        probe.listen(0, '127.0.0.1', () => {
            const { port } = probe.address();
            probe.close(() => resolve(port));
        });
    });
}

async function waitFor(predicate, message, timeoutMs = 15000) {
    const deadline = Date.now() + timeoutMs;
    let last;
    while (Date.now() < deadline) {
        last = await predicate();
        if (last) return last;
        await delay(250);
    }
    throw new Error(`${message} (timed out after ${timeoutMs / 1000}s)`);
}

try {
    let binary = process.env.KULA_BINARY ? path.resolve(process.env.KULA_BINARY) : '';
    if (!binary) {
        binary = path.join(scratch, 'kula');
        const build = spawnSync('go', ['build', '-o', binary, './cmd/kula'], {
            cwd: root, stdio: 'inherit', env: { ...process.env, CGO_ENABLED: '0' },
        });
        if (build.status !== 0) throw new Error('go build ./cmd/kula failed');
    }

    const port = await freePort();
    const base = `http://127.0.0.1:${port}`;
    fs.mkdirSync(path.join(scratch, 'data'));
    server = spawn(binary, ['-config', path.join(root, 'config.example.yaml'), 'serve'], {
        cwd: scratch,
        stdio: ['ignore', 'pipe', 'pipe'],
        env: { ...process.env, KULA_DIRECTORY: path.join(scratch, 'data'),
            KULA_LISTEN: '127.0.0.1', KULA_PORT: String(port) },
    });
    serverClosed = new Promise(resolve => server.once('close', resolve));
    const capture = chunk => { serverOutput = (serverOutput + chunk).slice(-16384); };
    server.stdout.on('data', capture);
    server.stderr.on('data', capture);
    await waitFor(async () => {
        if (server.exitCode !== null) throw new Error(`kula serve exited early:\n${serverOutput}`);
        return fetch(`${base}/health`).then(response => response.ok, () => false);
    }, 'kula serve did not become healthy');
    // Let the collector write a few observations before the first history load.
    await delay(3000);

    const userDataDir = path.join(scratch, 'chrome');
    let endpoint;
    ({ browser, browserClosed, endpoint } = await launchChromium(browserPath, [
        '--headless=new', '--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage',
        '--disable-background-networking', '--no-first-run', '--remote-debugging-port=0',
        `--user-data-dir=${userDataDir}`, 'about:blank',
    ], userDataDir));
    socket = new WebSocket(endpoint);
    await new Promise(resolve => socket.addEventListener('open', resolve, { once: true }));
    let id = 0;
    const pending = new Map();
    let historyRequests = 0;
    const historyUrls = [];
    socket.addEventListener('message', event => {
        const message = JSON.parse(event.data);
        if (pending.has(message.id)) {
            const { resolve, reject } = pending.get(message.id);
            pending.delete(message.id);
            if (message.error) reject(new Error(JSON.stringify(message.error)));
            else resolve(message.result);
        } else if (message.method === 'Network.requestWillBeSent' &&
            message.params.request.url.includes('/api/history?')) {
            historyRequests++;
            historyUrls.push(new URL(message.params.request.url));
        } else if (message.method === 'Runtime.exceptionThrown') {
            errors.push(message.params.exceptionDetails?.exception?.description ||
                message.params.exceptionDetails?.text);
        }
    });
    const send = (method, params = {}, sessionId) => new Promise((resolve, reject) => {
        const next = ++id;
        pending.set(next, { resolve, reject });
        socket.send(JSON.stringify({ id: next, method, params, ...(sessionId ? { sessionId } : {}) }));
    });
    const { targetId } = await send('Target.createTarget', { url: 'about:blank' });
    const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true });
    const call = (method, params) => send(method, params, sessionId);
    await call('Network.enable');
    await call('Runtime.enable');
    const evaluate = async expression => {
        const out = await call('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
        if (out.exceptionDetails) throw new Error(JSON.stringify(out.exceptionDetails));
        return out.result.value;
    };
    const resize = (width, height) => call('Emulation.setDeviceMetricsOverride',
        { width, height, deviceScaleFactor: 1, mobile: false });

    // CPU total usage is the principal Min–Max series of the first chart.
    const snapshot = () => evaluate(`(() => {
        if (typeof Chart === 'undefined' || !document.getElementById('btn-agg-menu')) return { points: 0 };
        const chart = Chart.getChart(document.getElementById('chart-cpu'));
        const points = chart?.data?.datasets?.[4]?.data ?? [];
        const drawn = points.filter(point => point.y != null);
        return {
            sampling: document.getElementById('sampling-info')?.textContent ?? '',
            aggregation: document.querySelector('#agg-presets-list .time-btn.active')?.dataset.agg ?? null,
            choices: !document.getElementById('btn-agg-menu').classList.contains('hidden'),
            urlAggregation: new URL(location.href).searchParams.get('agg'),
            points: points.length,
            blank: points.length - drawn.length,
            unavailable: points.filter(point => point.extremaUnavailable).length,
            lastX: drawn.at(-1)?.x ?? null,
            lastAge: drawn.length ? (Date.now() - drawn.at(-1).x) / 1000 : null,
        };
    })()`);
    const check = (condition, message, state) => {
        if (!condition) throw new Error(`${message}: ${JSON.stringify(state)}`);
    };
    // Bucketed rolling views refresh their trailing buckets, not the window:
    // every refresh after the first snapshot spans a few output steps, far
    // less than the window it extends.
    const tailRefreshes = (since, rangeSeconds) => historyUrls.slice(since).filter(url =>
        Date.parse(url.searchParams.get('to')) - Date.parse(url.searchParams.get('from')) <
            rangeSeconds * 1000 / 4).length;
    const selectRange = seconds => evaluate(`document.querySelector('.time-btn[data-range="${seconds}"]').click()`);
    const settle = async (predicate, message) => waitFor(async () => {
        const state = await snapshot();
        return state.points > 0 && predicate(state) ? state : null;
    }, message);

    // A phone-width 5-minute view is answered in 2s buckets with extrema. It
    // must keep Max, refresh the snapshot, and never append blank live points.
    await resize(380, 800);
    await call('Page.navigate', { url: `${base}/?range=300&agg=max` });
    let state = await settle(s => /^2s\b/.test(s.sampling), 'phone view did not load 2s buckets');
    check(state.aggregation === 'max' && state.choices, 'phone view lost the Max selection', state);
    let requests = historyRequests;
    const phonePoints = state.points;
    await delay(10000);
    state = await snapshot();
    check(historyRequests - requests >= 2, 'bucketed phone view did not refresh its snapshot', state);
    check(tailRefreshes(requests, 300) === historyRequests - requests,
        'bucketed phone view refetched its whole window', { ...state, urls: historyUrls.slice(requests).map(String) });
    check(state.blank === 0 && state.unavailable === 0, 'Max drew blank points', state);
    check(state.lastAge <= 6, 'bucketed Max view stopped advancing', state);
    check(state.points >= phonePoints && state.points <= phonePoints + 8,
        'tail refreshes did not extend the phone view by its new buckets', { ...state, phonePoints });
    const bucketed = state;

    // A desktop 5-minute view is raw: no Min/Max, and live samples stream in
    // without history requests.
    await resize(1920, 1080);
    await delay(300);
    await selectRange(300);
    state = await settle(s => /^1s\b/.test(s.sampling), 'desktop view did not load raw history');
    check(state.aggregation === 'avg' && !state.choices, 'raw desktop view offered Min/Max', state);
    requests = historyRequests;
    const streamedFrom = state.lastX;
    await delay(6000);
    state = await snapshot();
    check(historyRequests === requests, 'raw desktop view refreshed instead of streaming', state);
    check(state.lastX - streamedFrom >= 4000 && state.lastAge <= 2.5, 'raw desktop view stopped streaming', state);
    const raw = state;

    // Narrowing again restores the suspended Max choice.
    await resize(380, 800);
    await delay(300);
    await selectRange(300);
    state = await settle(s => /^2s\b/.test(s.sampling), 'phone view did not return to 2s buckets');
    check(state.aggregation === 'max' && state.urlAggregation === 'max', 'Max was not restored', state);

    // A desktop hour is bucketed from raw data: Max remains available and the
    // rolling snapshot keeps every bucket drawn.
    await resize(1920, 1080);
    await delay(300);
    await selectRange(3600);
    state = await settle(s => !/^1s\b/.test(s.sampling), 'desktop hour did not load bucketed history');
    check(state.aggregation === 'max' && state.choices, 'bucketed desktop hour lost Max', state);
    requests = historyRequests;
    await delay(10000);
    state = await snapshot();
    check(historyRequests > requests, 'bucketed desktop hour did not refresh its snapshot', state);
    check(state.blank === 0 && state.unavailable === 0 && state.lastAge <= 15,
        'bucketed desktop hour stopped advancing in Max', state);
    // The first refresh after a resize may be whole: the settled layout gives
    // a new point budget. With the budget stable, the hour refreshes its tail.
    const settled = historyRequests;
    await waitFor(() => historyRequests > settled, 'bucketed desktop hour stopped refreshing', 25000);
    state = await snapshot();
    check(tailRefreshes(settled, 3600) === historyRequests - settled,
        'bucketed desktop hour refetched its whole window', { ...state, urls: historyUrls.slice(settled).map(String) });
    check(state.blank === 0 && state.unavailable === 0 && state.lastAge <= 15,
        'tail refresh left the desktop hour blank in Max', state);

    const hour = state;

    // TV mode is restored over a stored Focus Mode selection, fits every card
    // on screen, keeps streaming under a resting cursor, and Escape leaves it.
    await evaluate(`localStorage.setItem('kula_focus_visible',
        JSON.stringify(['card-cpu', 'card-memory', 'card-network'])); localStorage.setItem('kula_focus_tv', 'true')`);
    await call('Page.navigate', { url: `${base}/?range=300` });
    const tvLayout = () => evaluate(`(() => {
        const grid = document.getElementById('charts-grid');
        if (!grid || !document.getElementById('btn-pause')) return null;
        const cards = Array.from(grid.children)
            .filter(card => card.classList.contains('chart-card') && card.getClientRects().length)
            .map(card => card.getBoundingClientRect().toJSON());
        return {
            tv: document.documentElement.classList.contains('tv-mode'),
            rows: grid.style.getPropertyValue('--tv-rows'),
            stored: localStorage.getItem('kula_focus_tv'),
            focus: document.getElementById('btn-focus').classList.contains('focus-active'),
            controls: document.querySelector('.time-controls').getClientRects().length > 0,
            paused: document.getElementById('btn-pause').classList.contains('paused'),
            overflow: document.scrollingElement.scrollHeight - innerHeight,
            cards, width: innerWidth, height: innerHeight,
        };
    })()`);
    let tv = await waitFor(async () => {
        const layout = await tvLayout();
        return layout?.tv && layout.rows && layout.cards.length === 3 ? layout : null;
    }, 'TV mode was not restored over the Focus Mode selection');
    check(!tv.controls && tv.overflow <= 0 && tv.cards.every(card =>
        card.bottom <= tv.height && card.right <= tv.width && card.height > 200),
    'TV mode did not fill the screen with the selection', tv);
    await settle(s => s.lastAge <= 2.5, 'TV mode did not load live history');
    const cpu = tv.cards[0];
    await call('Input.dispatchMouseEvent', { type: 'mouseMoved',
        x: cpu.x + cpu.width / 2, y: cpu.y + cpu.height / 2 });
    const rested = await snapshot();
    await delay(4000);
    state = await snapshot();
    tv = await tvLayout();
    check(!tv.paused && state.lastX - rested.lastX >= 2000, 'a resting cursor paused TV mode', { tv, state });
    await call('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
    await call('Input.dispatchKeyEvent', { type: 'keyUp', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
    tv = await waitFor(async () => {
        const layout = await tvLayout();
        return layout && !layout.tv ? layout : null;
    }, 'Escape did not leave TV mode');
    check(tv.stored === null && tv.focus && tv.controls && tv.cards.length === 3,
        'leaving TV mode did not return to Focus Mode', tv);

    // Rearranging with real input: a mouse drag on a card body moves Processes
    // to the top and the Swap gauge to the end without touching the charts,
    // the order survives a reload and drives Focus mode, Escape abandons a
    // drag, a grip takes arrow keys, and Reset brings back the template order.
    await resize(1920, 2600);
    await evaluate(`localStorage.removeItem('kula_focus_visible'); localStorage.removeItem('kula_focus_tv');
        localStorage.removeItem('kula_card_order')`);
    await call('Page.navigate', { url: `${base}/?range=300` });
    await settle(s => s.lastAge <= 2.5, 'the dashboard did not load for rearranging');
    const cardLayout = () => evaluate(`(() => {
        window.scrollTo(0, 0);
        const ids = selector => Array.from(document.querySelectorAll(selector))
            .filter(card => card.getClientRects().length).map(card => card.id);
        const rects = {};
        document.querySelectorAll('[data-card-section]').forEach(card => {
            if (card.getClientRects().length) rects[card.id] = card.getBoundingClientRect().toJSON();
        });
        const stored = localStorage.getItem('kula_card_order');
        return {
            system: ids('#charts-grid > .chart-card'),
            gauges: ids('#gauges-row > .gauge-card'),
            visible: ids('#charts-grid > .chart-card.focus-visible'),
            rects,
            stored: stored === null ? null : JSON.parse(stored),
            rearranging: !!document.getElementById('rearrange-bar'),
            search: location.search,
        };
    })()`);
    const startRearranging = async () => {
        await evaluate(`document.getElementById('btn-settings').click(); document.getElementById('btn-rearrange').click()`);
        await waitFor(() => evaluate(`!!document.querySelector('#rearrange-bar') &&
            !!document.querySelector('#card-cpu > .card-grip')`), 'Rearrange mode did not start');
    };
    const mouse = (type, x, y) => call('Input.dispatchMouseEvent', {
        type, x, y, button: 'left', buttons: type === 'mouseReleased' ? 0 : 1, clickCount: 1,
    });
    const centre = rect => ({ x: rect.x + rect.width / 2, y: rect.y + rect.height / 2 });
    const dragMouse = async (from, to, release = true) => {
        await call('Input.dispatchMouseEvent', { type: 'mouseMoved', x: from.x, y: from.y });
        await mouse('mousePressed', from.x, from.y);
        for (let step = 1; step <= 12; step++) {
            await mouse('mouseMoved', from.x + (to.x - from.x) * step / 12, from.y + (to.y - from.y) * step / 12);
            await delay(40);
        }
        if (release) await mouse('mouseReleased', to.x, to.y);
        await delay(300);
    };
    const key = async (name, code, keyCode) => {
        await call('Input.dispatchKeyEvent', { type: 'keyDown', key: name, code, windowsVirtualKeyCode: keyCode });
        await call('Input.dispatchKeyEvent', { type: 'keyUp', key: name, code, windowsVirtualKeyCode: keyCode });
        await delay(200);
    };

    let cards = await cardLayout();
    const templateSystem = cards.system;
    const templateGauges = cards.gauges;
    check(templateSystem[0] === 'card-cpu' && templateGauges.at(-1) === 'gauge-ul' && cards.stored === null,
        'the dashboard did not start in template order', cards);
    await startRearranging();
    cards = await cardLayout();
    const cpuRect = cards.rects['card-cpu'];
    await dragMouse(centre(cards.rects['card-processes']), { x: cpuRect.x + cpuRect.width / 4, y: centre(cpuRect).y });
    cards = await cardLayout();
    check(cards.system[0] === 'card-processes' && cards.system[1] === 'card-cpu' &&
        cards.stored?.system?.[0] === 'card-processes', 'dragging Processes onto CPU did not move it first', cards);
    check(cards.search === '?range=300', 'dragging a card zoomed its chart', cards);
    const lastGauge = cards.rects[cards.gauges.at(-1)];
    await dragMouse(centre(cards.rects['gauge-swap']), { x: lastGauge.x + lastGauge.width - 4, y: centre(lastGauge).y });
    cards = await cardLayout();
    check(cards.gauges.at(-1) === 'gauge-swap' && cards.stored?.gauges?.at(-1) === 'gauge-swap',
        'dragging the Swap gauge to the end did not move it', cards);
    // Escape abandons a drag in progress; nothing is stored.
    const beforeEscape = cards;
    await dragMouse(centre(cards.rects['card-memory']), centre(cards.rects['card-processes']), false);
    await key('Escape', 'Escape', 27);
    await mouse('mouseReleased', centre(cards.rects['card-processes']).x, centre(cards.rects['card-processes']).y);
    await delay(300);
    cards = await cardLayout();
    check(cards.system.join() === beforeEscape.system.join() && JSON.stringify(cards.stored) === JSON.stringify(beforeEscape.stored) &&
        cards.rearranging, 'Escape did not abandon the drag', cards);
    // A grip takes real arrow keys.
    await evaluate(`document.querySelector('#card-cpu > .card-grip').focus()`);
    await key('ArrowRight', 'ArrowRight', 39);
    cards = await cardLayout();
    check(cards.system.indexOf('card-cpu') === 2, 'ArrowRight on a grip did not move CPU', cards);
    await evaluate(`document.getElementById('btn-rearrange-done').click()`);

    await call('Page.navigate', { url: `${base}/?range=300` });
    await settle(s => s.lastAge <= 2.5, 'the dashboard did not reload after rearranging');
    cards = await cardLayout();
    check(cards.system[0] === 'card-processes' && cards.system.indexOf('card-cpu') === 2 &&
        cards.gauges.at(-1) === 'gauge-swap' && !cards.rearranging, 'the order did not survive a reload', cards);
    await evaluate(`localStorage.setItem('kula_focus_visible', JSON.stringify(['card-cpu', 'card-processes']))`);
    await call('Page.navigate', { url: `${base}/?range=300` });
    await settle(s => s.lastAge <= 2.5, 'Focus mode did not load');
    cards = await waitFor(async () => {
        const layout = await cardLayout();
        return layout.visible.length === 2 ? layout : null;
    }, 'Focus mode did not show the selection');
    check(cards.visible.join() === 'card-processes,card-cpu', 'Focus mode ignored the stored order', cards);
    await evaluate(`localStorage.removeItem('kula_focus_visible')`);
    await call('Page.navigate', { url: `${base}/?range=300` });
    await settle(s => s.lastAge <= 2.5, 'the dashboard did not reload before Reset');
    await startRearranging();
    await evaluate(`document.getElementById('btn-rearrange-reset').click(); document.getElementById('btn-rearrange-done').click()`);
    cards = await cardLayout();
    check(cards.system.join() === templateSystem.join() && cards.gauges.join() === templateGauges.join() &&
        cards.stored === null, 'Reset order did not restore the template order', cards);
    await resize(1920, 1080);

    // Blocked site data makes every localStorage access throw, as it does from
    // here on for this page. The dashboard still starts and streams, and its
    // toggles work for the page; so does the game, where a packaged build
    // still serves it.
    await call('Page.enable');
    await call('Page.addScriptToEvaluateOnNewDocument', { source: `Object.defineProperty(window, 'localStorage', {
        configurable: true,
        get() { throw new DOMException('The operation is insecure.', 'SecurityError'); },
    });` });
    await call('Page.navigate', { url: `${base}/?range=300` });
    const storage = await evaluate(`(() => {
        try { return typeof localStorage.getItem; } catch (error) { return error.name; }
    })()`);
    check(storage === 'SecurityError', 'localStorage was not blocked', { storage });
    state = await settle(s => s.lastAge <= 2.5, 'the dashboard did not start with storage blocked');
    const blocked = await evaluate(`(() => {
        const light = document.body.classList.contains('light-mode');
        document.getElementById('btn-layout').click();
        document.getElementById('btn-theme').click();
        return {
            list: document.getElementById('dashboard').classList.contains('layout-list'),
            themed: document.body.classList.contains('light-mode') !== light,
        };
    })()`);
    check(blocked.list && blocked.themed, 'layout and theme toggles failed with storage blocked', blocked);
    const blockedMove = await evaluate(`(() => {
        document.getElementById('btn-rearrange').click();
        const grip = document.querySelector('#card-cpu > .card-grip');
        grip.focus();
        grip.dispatchEvent(new KeyboardEvent('keydown', { key: 'End', bubbles: true, cancelable: true }));
        const system = Array.from(document.querySelectorAll('#charts-grid > .chart-card'))
            .filter(card => card.getClientRects().length).map(card => card.id);
        document.getElementById('btn-rearrange-done').click();
        return system.at(-1);
    })()`);
    check(blockedMove === 'card-cpu', 'rearranging failed with storage blocked', { blockedMove });
    const game = await evaluate(`fetch('game.html').then(r => r.ok ? r.text() : '').then(t => t.includes('game.js'))`);
    if (game) {
        await call('Page.navigate', { url: `${base}/game.html` });
        await waitFor(() => evaluate(`document.readyState === 'complete' &&
            !!document.getElementById('start-screen')`), 'the game did not load with storage blocked');
        const muted = await evaluate(`(() => {
            const button = document.getElementById('btn-mute');
            button.click();
            return button.style.opacity;
        })()`);
        check(muted === '0.5', 'the game did not start with storage blocked', { muted });
    }

    assert.deepEqual(errors, [], 'the dashboard threw exceptions');
    console.log(JSON.stringify({ status: 'pass', bucketed_phone: bucketed.sampling,
        raw_desktop: raw.sampling, desktop_hour: hour.sampling, history_requests: historyRequests }));
} catch (error) {
    if (serverOutput) console.error(`kula serve output:\n${serverOutput}`);
    if (errors.length) console.error(`dashboard exceptions:\n${errors.join('\n')}`);
    throw error;
} finally {
    socket?.close();
    await stopProcess(browser, browserClosed);
    await stopProcess(server, serverClosed);
    fs.rmSync(scratch, { recursive: true, force: true });
}
