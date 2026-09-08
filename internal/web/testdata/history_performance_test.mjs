// Run with: node internal/web/testdata/history_performance_test.mjs
// Uses the production chart modules from history_performance.html and fixes
// the viewport before navigation so Chromium's --dump-dom startup race cannot
// expose a transient 0x0 window to the culling assertions.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import assert from 'node:assert/strict';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { delay, findChromium, launchChromium } from './chromium_test_helper.mjs';

const fixture = fileURLToPath(new URL('./history_performance.html', import.meta.url));
const browserPath = findChromium();
if (!browserPath) throw new Error('Chromium/Chrome not found; set KULA_CHROMIUM');
if (typeof WebSocket !== 'function') throw new Error('Node.js 22+ is required');
const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'kula-history-performance-'));
const userDataDir = `${scratch}/chrome`;
let browser, browserClosed, socket;

try {
    let endpoint;
    ({ browser, browserClosed, endpoint } = await launchChromium(browserPath, [
        '--headless=new',
        '--no-sandbox',
        '--disable-gpu',
        '--disable-dev-shm-usage',
        '--disable-background-networking',
        '--allow-file-access-from-files',
        '--enable-precise-memory-info',
        '--remote-debugging-port=0',
        `--user-data-dir=${userDataDir}`,
        'about:blank',
    ], userDataDir));
    socket = new WebSocket(endpoint);
    await new Promise(resolve => socket.addEventListener('open', resolve, { once: true }));
    let id = 0;
    const pending = new Map();
    const runtimeErrors = [];
    socket.addEventListener('message', event => {
        const message = JSON.parse(event.data);
        if (message.method === 'Runtime.exceptionThrown') {
            runtimeErrors.push(message.params?.exceptionDetails?.text || 'Unreported browser exception');
        }
        if (!pending.has(message.id)) return;
        const { resolve, reject } = pending.get(message.id);
        pending.delete(message.id);
        if (message.error) reject(new Error(JSON.stringify(message.error)));
        else resolve(message.result);
    });
    const send = (method, params = {}, sessionId) => new Promise((resolve, reject) => {
        const next = ++id;
        pending.set(next, { resolve, reject });
        socket.send(JSON.stringify({ id: next, method, params, ...(sessionId ? { sessionId } : {}) }));
    });
    const { targetId } = await send('Target.createTarget', { url: 'about:blank' });
    const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true });
    const call = (method, params = {}) => send(method, params, sessionId);
    const evaluate = async expression => {
        const output = await call('Runtime.evaluate', { expression, returnByValue: true });
        if (output.exceptionDetails) throw new Error(JSON.stringify(output.exceptionDetails));
        return output.result.value;
    };

    await call('Runtime.enable');
    await call('Emulation.setDeviceMetricsOverride', {
        width: 800,
        height: 600,
        deviceScaleFactor: 1,
        mobile: false,
    });
    await call('Page.navigate', { url: pathToFileURL(fixture).href });

    let status;
    for (let i = 0; i < 400; i++) {
        status = await evaluate('document.body?.dataset?.status || null');
        if (status) break;
        if (runtimeErrors.length > 0) throw new Error(runtimeErrors.join('; '));
        await delay(25);
    }
    assert.ok(status, 'Performance fixture timed out');
    const result = await evaluate('JSON.parse(document.getElementById("results").textContent)');
    assert.equal(result.status, 'pass', result.error || JSON.stringify(result));
    assert.equal(result.charts, 46);
    assert.equal(result.points, 165600);
    assert.ok(result.charts_updated_in_viewport > 0);
    assert.ok(result.charts_updated_in_viewport < result.charts);
    assert.equal(result.charts_rendered_for_crosshair, result.charts_updated_in_viewport);
    assert.equal(result.keyboard_gesture, true);
    assert.equal(result.timezone_tooltip, true);
    assert.equal(result.accessible_alternatives, true);
    assert.equal(result.data_controls_off_by_default, true);
    assert.equal(result.data_panel_lazy, true);
    assert.deepEqual(runtimeErrors, []);
    console.log(JSON.stringify(result));
} finally {
    socket?.close();
    if (browser?.exitCode === null && browser.signalCode === null) browser.kill('SIGTERM');
    if (browserClosed) await browserClosed;
    fs.rmSync(scratch, { recursive: true, force: true });
}
