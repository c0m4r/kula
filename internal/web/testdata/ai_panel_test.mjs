// Run with: node --experimental-websocket internal/web/testdata/ai_panel_test.mjs
// Starts the real `kula serve` with the AI assistant pointed at a fake Ollama
// on loopback, and drives the dashboard's AI panel in Chromium: an answer
// streams and renders, a chat runs on the model picked in the panel, a
// failing backend shows only a generic message, and a 1 MiB answer in 8-byte
// chunks keeps the page responsive and is cut with a notice, after which the
// conversation goes on. Set KULA_BINARY to reuse a built binary; otherwise
// the test builds one.
import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { cspViolation, delay, findChromium, launchChromium, stopProcess, watchCSPViolations } from './chromium_test_helper.mjs';

const root = fileURLToPath(new URL('../../../', import.meta.url));
const browserPath = findChromium();
if (!browserPath) throw new Error('Chromium/Chrome not found; set KULA_CHROMIUM');
if (typeof WebSocket !== 'function') throw new Error('Node.js 22+ is required');

const { AI_MAX_ANSWER } = await import(`data:text/javascript;base64,${Buffer.from(
    fs.readFileSync(path.join(root, 'internal/web/static/js/app/ai-stream.js'))).toString('base64')}`);
const en = JSON.parse(fs.readFileSync(path.join(root, 'internal/i18n/locales/en.json'), 'utf8'));

const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'kula-ai-test-'));
let server, serverClosed, browser, browserClosed, socket, backend;
let serverOutput = '';
const errors = [];
const cspViolations = [];

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

// A page stuck in a long task answers no evaluation, so the deadline does
// not wait for the predicate.
async function waitFor(predicate, message, timeoutMs = 15000) {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
        const last = await Promise.race([predicate(), delay(deadline - Date.now()).then(() => false)]);
        if (last) return last;
        await delay(100);
    }
    throw new Error(`${message} (timed out after ${timeoutMs / 1000}s)`);
}

// A fake Ollama: the last user turn picks the answer. Every chat it receives
// is recorded.
const ANSWER = '✅ **CPU** is fine.\n\n| metric | value |\n|---|---|\n| load | 0.42 |\n\nRun `uptime`.';
const chats = [];
function ndjson(res, chunks) {
    res.writeHead(200, { 'Content-Type': 'application/x-ndjson' });
    for (const content of chunks) res.write(`${JSON.stringify({ message: { content }, done: false })}\n`);
    res.end(`${JSON.stringify({ message: { content: '' }, done: true })}\n`);
}
function fakeOllama(req, res) {
    if (req.method === 'GET' && req.url === '/api/tags') {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        return res.end(JSON.stringify({ models: [{ name: 'm1' }, { name: 'm2:latest' }] }));
    }
    if (req.method !== 'POST' || req.url !== '/api/chat') {
        res.writeHead(404);
        return res.end();
    }
    let body = '';
    req.setEncoding('utf8');
    req.on('data', chunk => { body += chunk; });
    req.on('end', () => {
        const chat = JSON.parse(body);
        chats.push(chat);
        const last = chat.messages.findLast(m => m.role === 'user')?.content ?? '';
        if (last === 'fail') {
            res.writeHead(500, { 'Content-Type': 'text/plain' });
            return res.end(`backend exploded at ${req.headers.host}`);
        }
        if (last === 'long') {
            const piece = 'word **b** `c` | x |\n';
            const text = piece.repeat(Math.ceil((1 << 20) / piece.length)).slice(0, 1 << 20);
            const chunks = [];
            for (let i = 0; i < text.length; i += 8) chunks.push(text.slice(i, i + 8));
            return ndjson(res, chunks);
        }
        const chunks = [];
        for (let i = 0; i < ANSWER.length; i += 7) chunks.push(ANSWER.slice(i, i + 7));
        ndjson(res, chunks);
    });
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

    backend = http.createServer(fakeOllama);
    await new Promise(resolve => backend.listen(0, '127.0.0.1', resolve));
    const backendURL = `http://127.0.0.1:${backend.address().port}`;

    const port = await freePort();
    const base = `http://127.0.0.1:${port}`;
    const data = path.join(scratch, 'data');
    fs.mkdirSync(data);
    const configPath = path.join(scratch, 'config.yaml');
    fs.writeFileSync(configPath, `storage:\n  directory: ${JSON.stringify(data)}\n` +
        `web:\n  listen: 127.0.0.1\n  port: ${port}\n` +
        `applications:\n  containers:\n    enabled: false\n` +
        `ollama:\n  enabled: true\n  url: ${JSON.stringify(backendURL)}\n  model: m1\n  timeout: 60s\n`);
    server = spawn(binary, ['-config', configPath, 'serve'], { cwd: scratch, stdio: ['ignore', 'pipe', 'pipe'] });
    serverClosed = new Promise(resolve => server.once('close', resolve));
    const capture = chunk => { serverOutput = (serverOutput + chunk).slice(-16384); };
    server.stdout.on('data', capture);
    server.stderr.on('data', capture);
    await waitFor(async () => {
        if (server.exitCode !== null) throw new Error(`kula serve exited early:\n${serverOutput}`);
        return fetch(`${base}/health`).then(response => response.ok, () => false);
    }, 'kula serve did not become healthy');

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
    socket.addEventListener('message', event => {
        const message = JSON.parse(event.data);
        if (pending.has(message.id)) {
            const { resolve, reject } = pending.get(message.id);
            pending.delete(message.id);
            if (message.error) reject(new Error(JSON.stringify(message.error)));
            else resolve(message.result);
        } else if (message.method === 'Runtime.exceptionThrown') {
            errors.push(message.params.exceptionDetails?.exception?.description ||
                message.params.exceptionDetails?.text);
        } else if (cspViolation(message)) {
            cspViolations.push(cspViolation(message));
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
    await call('Runtime.enable');
    await watchCSPViolations(call);
    const evaluate = async expression => {
        const out = await call('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
        if (out.exceptionDetails) throw new Error(JSON.stringify(out.exceptionDetails));
        return out.result.value;
    };

    await call('Page.navigate', { url: `${base}/` });
    await waitFor(() => evaluate(`(() => {
        const button = document.getElementById('btn-ai');
        return !!button && !button.classList.contains('hidden');
    })()`), 'the AI button did not appear');
    // The longest the main thread kept a 10 ms timer waiting.
    await evaluate(`(() => {
        window.__kulaGap = { max: 0, last: performance.now() };
        setInterval(() => {
            const now = performance.now();
            window.__kulaGap.max = Math.max(window.__kulaGap.max, now - window.__kulaGap.last);
            window.__kulaGap.last = now;
        }, 10);
    })()`);
    await evaluate(`document.getElementById('btn-ai').click()`);
    await waitFor(() => evaluate(`document.getElementById('ai-model-select').options.length === 2`),
        'the panel did not list the backend\'s models');

    // Ask in the panel and wait for the answer to end; returns the last
    // answer's markup, text and notices.
    const ask = async (prompt, timeoutMs = 15000) => {
        const asked = chats.length;
        await evaluate(`(() => {
            window.__kulaGap.max = 0;
            window.__kulaGap.last = performance.now();
            document.getElementById('ai-input').value = ${JSON.stringify(prompt)};
            document.getElementById('btn-ai-send').click();
        })()`);
        await waitFor(() => chats.length > asked, `${prompt}: no chat reached the backend`);
        await waitFor(() => evaluate(`!document.getElementById('btn-ai-send').disabled`),
            `${prompt}: the answer did not end`, timeoutMs);
        return evaluate(`(() => {
            const bodies = document.querySelectorAll('#ai-messages .ai-msg-assistant .ai-msg-body');
            const body = bodies[bodies.length - 1];
            return {
                html: body.innerHTML, text: body.textContent, typing: body.classList.contains('ai-typing'),
                cut: body.querySelector('.ai-truncated')?.textContent ?? null,
                error: body.querySelector('.ai-error')?.textContent ?? null,
                gap: window.__kulaGap.max,
            };
        })()`);
    };

    // An answer streams in small chunks and renders as markdown; the chat
    // carried only what the server keeps, on the configured model.
    let answer = await ask('hello');
    assert.match(answer.html, /<strong>CPU<\/strong>/);
    assert.match(answer.html, /<table class="ai-table">.*<td>0\.42<\/td>/s);
    assert.match(answer.html, /<code>uptime<\/code>/);
    assert.equal(answer.typing, false, 'the typing cursor stayed');
    assert.equal(answer.cut, null);
    assert.equal(chats.at(-1).model, 'm1');
    assert.deepEqual(chats.at(-1).messages.map(m => m.role), ['system', 'user']);

    // The model picked in the panel is the one the chat runs on, and the
    // conversation so far goes with it.
    await evaluate(`(() => {
        const select = document.getElementById('ai-model-select');
        select.value = 'm2:latest';
        select.dispatchEvent(new Event('change'));
    })()`);
    answer = await ask('and the load?');
    assert.match(answer.html, /<td>0\.42<\/td>/);
    assert.equal(chats.at(-1).model, 'm2:latest');
    assert.deepEqual(chats.at(-1).messages.map(m => m.role), ['system', 'user', 'assistant', 'user']);
    assert.equal(chats.at(-1).messages[2].content, ANSWER);

    // A failing backend: the panel shows a generic message, never the
    // backend's address or answer.
    answer = await ask('fail');
    assert.ok(answer.error, 'no error shown');
    assert.doesNotMatch(answer.error, /127\.0\.0\.1|exploded/);
    assert.equal(answer.typing, false);

    // 1 MiB in 8-byte chunks: repainting the whole answer per chunk froze the
    // tab for minutes. It is cut at AI_MAX_ANSWER with a notice instead.
    const started = Date.now();
    const long = await ask('long', 60000);
    const took = Date.now() - started;
    assert.equal(long.cut, en.ai_answer_truncated, 'no notice on the cut answer');
    assert.ok(long.text.length <= AI_MAX_ANSWER + long.cut.length,
        `${long.text.length} characters shown, over the cap`);
    assert.ok(long.text.startsWith('word b c | x |'), 'the cut answer is not rendered');
    assert.ok(long.gap < 1000, `the page stopped for ${Math.round(long.gap)} ms`);

    // The conversation goes on: the cut answer is sent back as much as the
    // server keeps of it, so the request stays under the server's body cap.
    answer = await ask('go on');
    assert.equal(answer.error, null, `the next question failed: ${answer.error}`);
    assert.match(answer.html, /<td>0\.42<\/td>/);
    const turns = chats.at(-1).messages;
    assert.equal(turns.at(-1).content, 'go on');
    assert.ok(turns.every(m => m.role !== 'assistant' || m.content.length <= 8000), 'an answer was sent whole');

    assert.deepEqual(errors, [], 'the dashboard threw exceptions');
    assert.deepEqual(cspViolations, [], 'the page broke its Content-Security-Policy');
    console.log(JSON.stringify({ status: 'pass', chats: chats.length, longAnswerMs: took, longestGapMs: Math.round(long.gap) }));
} catch (error) {
    if (serverOutput) console.error(`kula serve output:\n${serverOutput}`);
    if (errors.length) console.error(`dashboard exceptions:\n${errors.join('\n')}`);
    if (cspViolations.length) console.error(`CSP violations:\n${cspViolations.join('\n')}`);
    throw error;
} finally {
    socket?.close();
    await stopProcess(browser, browserClosed);
    await stopProcess(server, serverClosed);
    backend?.close();
    backend?.closeAllConnections();
    fs.rmSync(scratch, { recursive: true, force: true });
}
