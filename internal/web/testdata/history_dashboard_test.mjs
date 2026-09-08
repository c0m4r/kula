// Run with: node internal/web/testdata/history_dashboard_test.mjs
// Requires Chromium and permission to listen on localhost. Uses production
// frontend modules, a controlled metrics API, and accelerated browser time.
import http from 'node:http';
import fs from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import os from 'node:os';
import assert from 'node:assert/strict';
import { delay, findChromium, launchChromium } from './chromium_test_helper.mjs';
const root = fileURLToPath(new URL('../../../', import.meta.url));
const browserPath = findChromium();
if (!browserPath) throw new Error('Chromium/Chrome not found; set KULA_CHROMIUM');
if (typeof WebSocket !== 'function') throw new Error('Node.js 22+ is required');
function source(_variant, file) { return fs.readFileSync(path.join(root, file)); }
const baseSample = {"ts": "2026-09-05T13:42:14.735104207+03:00", "cpu": {"total": {"user": 0.5, "system": 0.99, "iowait": 0, "irq": 0, "softirq": 0, "steal": 0, "usage": 1.49}, "num_cores": 2}, "lavg": {"load1": 0.1, "load5": 0.09, "load15": 0.1, "running": 1, "total": 137}, "mem": {"total": 3996106752, "free": 2733215744, "available": 3330564096, "used": 334237696, "buffers": 130486272, "cached": 798167040, "shmem": 4939776, "used_pct": 8.36}, "swap": {"total": 4294963200, "free": 4294963200, "used": 0, "used_pct": 0}, "net": {"ifaces": [{"name": "eth0", "rx_bytes": 29812214, "tx_bytes": 879758176, "rx_mbps": 0, "tx_mbps": 0.02, "rx_pkts": 281049, "tx_pkts": 165479, "rx_pps": 3, "tx_pps": 3, "rx_errs": 0, "tx_errs": 0, "rx_drop": 0, "tx_drop": 0}], "tcp": {"curr_estab": 11, "in_errs_ps": 0, "out_rsts_ps": 0, "retrans_ps": 0}, "sockets": {"tcp_inuse": 11, "tcp_tw": 0, "udp_inuse": 1}}, "disk": {"devices": [{"name": "sda", "reads_ps": 0, "writes_ps": 2, "read_bps": 0, "write_bps": 16384.789156984956, "util_pct": 0}], "filesystems": [{"device": "/dev/sda1", "mount": "/", "fstype": "ext4", "total": 39973924864, "used": 17882783744, "available": 20406947840, "used_pct": 44.74}, {"device": "/dev/sda15", "mount": "/efi", "fstype": "vfat", "total": 264281088, "used": 148992, "available": 264132096, "used_pct": 0.06}]}, "sys": {"hostname": "test-host", "uptime_sec": 421356.07, "uptime_human": "4d 21h 2m", "entropy": 256, "clock_synced": true, "clock_source": "kvm-clock", "user_count": 1}, "proc": {"total": 121, "running": 0, "sleeping": 71, "zombie": 0, "blocked": 0, "threads": 137}, "self": {"cpu_pct": 0, "mem_rss": 86364160, "fds": 15}, "apps": {"nginx": {"active_conn": 4, "accepts": 7878, "handled": 7878, "requests": 428332, "accepts_ps": 0, "handled_ps": 0, "requests_ps": 1, "reading": 0, "writing": 4, "waiting": 0}}};
function history(url) {
    const count = Number(url.searchParams.get('points')) || 700;
    const end = Date.parse(url.searchParams.get('to'));
    const start = Date.parse(url.searchParams.get('from'));
    const gapFixture = url.searchParams.get('gaps') === 'true';
    const positions = Array.from({length: count}, (_, i) => i + Math.floor(i / 3) * 9);
    const span = positions.at(-1);
    const step = (end - start) / (count - 1);
    const samples = Array.from({ length: count }, (_, i) => {
        const data = structuredClone(baseSample);
        data.ts = new Date(start + (gapFixture ? positions[i] / span * (end - start) : i * step)).toISOString();
        data.cpu.total.user = 20 + 15 * Math.sin(i / 9);
        data.cpu.total.usage = data.cpu.total.user + 8;
        data.sys.uptime_sec = 500000 + i * step / 1000;
        const min = structuredClone(data), max = structuredClone(data);
        min.cpu.total.user *= 0.5; max.cpu.total.user *= 1.8;
        min.cpu.total.usage *= 0.5; max.cpu.total.usage *= 1.8;
        return { ts: data.ts, dur: step * 1e6, data, min, max,
            bucket_start: new Date(start + (i - 1) * step).toISOString(), bucket_end: data.ts,
            sample_count: Math.round(step / 1000), coverage: 1 };
    });
    return { samples, tier: 1, resolution: gapFixture ? '1s' : `${step / 1000}s`, source_resolution: '1m',
        downsampled: true,
        requested_from: new Date(start).toISOString(), requested_to: new Date(end).toISOString(),
        actual_from: new Date(start).toISOString(), actual_to: new Date(end).toISOString(),
        complete: true, exact_complete: true,
        valid_aggregations: ['data', 'min', 'max'] };
}
const client = fs.readFileSync(new URL('./history_dashboard_client.js', import.meta.url), 'utf8');
const server = http.createServer((req, res) => {
    try {
        const url = new URL(req.url, 'http://localhost');
        const [, variant, ...rest] = url.pathname.split('/');
        const relative = rest.join('/');
        const sendJSON = value => { res.setHeader('Content-Type', 'application/json'); res.end(JSON.stringify(value)); };
        if (relative === 'api/auth/status') return sendJSON({auth_required: false});
        if (relative === 'api/config') return sendJSON({ lang: { default: 'en' }, join_metrics: false,
            history: { collection_interval_ms: 1000, ranges: [
                { from: '2026-04-17T03:00:00Z', to: '2026-04-19T21:00:00Z' },
                { from: '2026-09-01T00:00:00Z', to: new Date().toISOString() },
            ] } });
        if (relative === 'api/i18n') {
            res.setHeader('Content-Type', 'application/json');
            const lang = url.searchParams.get('lang') === 'pl' ? 'pl' : 'en';
            return res.end(source(variant, `internal/i18n/locales/${lang}.json`));
        }
        if (relative === 'api/history') return sendJSON(history(url));
        if (relative === 'audit-client.js') { res.setHeader('Content-Type', 'text/javascript'); return res.end(client); }
        if (relative === '' || relative === 'index.html') {
            let html = source(variant, 'internal/web/static/index.html').toString();
            html = html.replace(/\{\{if \.BasePath\}\}[\s\S]*?\{\{end\}\}/g, `<base href="/${variant}/">`)
                .replace(/\{\{if \.AuthEnabled\}\}[\s\S]*?\{\{end\}\}/g, '')
                .replace(/\{\{if \.EasterEgg\}\}[\s\S]*?\{\{end\}\}/g, '')
                .replace(/window.KULA_BASE_PATH = \{\{\.BasePath\}\}/, `window.KULA_BASE_PATH = "/${variant}"`)
                .replace(/\s*integrity="[^"]*"/g, '').replace(/\{\{[^}]*\}\}/g, '')
                .replace(/<script type="module"[\s\S]*?<\/script>/, `<script type="module" src="audit-client.js"></script>`);
            res.setHeader('Content-Type', 'text/html'); return res.end(html);
        }
        const mime = { '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.woff2': 'font/woff2' };
        res.setHeader('Content-Type', mime[path.extname(relative)] || 'application/octet-stream');
        res.end(source(variant, `internal/web/static/${relative}`));
    } catch {
        res.statusCode = 404;
        res.setHeader('Content-Type', 'text/plain; charset=utf-8');
        res.end('Not found');
    }
});
await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
});
const port = server.address().port;
const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'kula-dashboard-test-'));
const userDataDir = `${scratch}/chrome`;
let browser, browserClosed, socket;
try {
    let endpoint;
    ({ browser, browserClosed, endpoint } = await launchChromium(browserPath, [
        '--headless=new', '--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage',
        '--disable-background-networking', '--no-first-run', '--remote-debugging-port=0',
        `--user-data-dir=${userDataDir}`, 'about:blank',
    ], userDataDir));
    socket = new WebSocket(endpoint);
    await new Promise(resolve => socket.addEventListener('open', resolve, { once: true }));
    let id = 0; const pending = new Map();
    socket.addEventListener('message', e => {
        const message = JSON.parse(e.data);
        if (pending.has(message.id)) {
            const { resolve, reject } = pending.get(message.id); pending.delete(message.id);
            if (message.error) reject(new Error(JSON.stringify(message.error))); else resolve(message.result);
        }
    });
    const send = (method, params = {}, sessionId) => new Promise((resolve, reject) => {
        const next = ++id; pending.set(next, { resolve, reject });
        socket.send(JSON.stringify({ id: next, method, params, ...(sessionId ? { sessionId } : {}) }));
    });
    const { targetId } = await send('Target.createTarget', { url: 'about:blank' });
    const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true });
    const call = (method, params) => send(method, params, sessionId);
    const evaluate = async expression => {
        const out = await call('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
        if (out.exceptionDetails) throw new Error(JSON.stringify(out.exceptionDetails));
        return out.result.value;
    };
    await call('Emulation.setDeviceMetricsOverride', { width: 1440, height: 1000, deviceScaleFactor: 1, mobile: false });
    await call('Page.navigate', { url: `http://127.0.0.1:${port}/fixture/` });
    let ready;
    for (let i = 0; i < 2400; i++) {
        ready = await evaluate('window.ready');
        const errors = await evaluate('window.errors');
        if (errors?.length) throw new Error(errors.join('; '));
        if (ready) break;
        await delay(50);
    }
    assert.ok(ready, 'Dashboard fixture timed out');
    const result = await evaluate('window.result');
    assert.equal(result.status, 'pass');
    console.log(JSON.stringify(result));
} finally {
    socket?.close();
    if (browser?.exitCode === null && browser.signalCode === null) browser.kill('SIGTERM');
    await new Promise(resolve => server.close(resolve));
    if (browserClosed) await browserClosed;
    fs.rmSync(scratch, {recursive: true, force: true});
}
