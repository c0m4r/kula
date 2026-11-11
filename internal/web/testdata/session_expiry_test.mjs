// Run with: node internal/web/testdata/session_expiry_test.mjs
// Starts the real `kula serve` with authentication, signs in through the
// dashboard in Chromium, then restarts the server without its saved sessions
// while the page stays open, as when a session expires while a laptop sleeps:
// the live socket drops and every reconnect is refused with 401, which a page
// sees only as a close before open. The dashboard must return to the login
// screen instead of retrying forever, and stream again after signing in. Set
// KULA_BINARY to reuse a built binary; otherwise the test builds one.
import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { cspViolation, delay, findChromium, launchChromium, stopProcess, watchCSPViolations } from './chromium_test_helper.mjs';

const root = fileURLToPath(new URL('../../../', import.meta.url));
const browserPath = findChromium();
if (!browserPath) throw new Error('Chromium/Chrome not found; set KULA_CHROMIUM');
if (typeof WebSocket !== 'function') throw new Error('Node.js 22+ is required');

const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'kula-session-test-'));
let server, serverClosed, browser, browserClosed, socket;
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
    const data = path.join(scratch, 'data');
    fs.mkdirSync(data);
    const configPath = path.join(scratch, 'config.yaml');
    const config = auth => `storage:\n  directory: ${JSON.stringify(data)}\n` +
        `web:\n  listen: 127.0.0.1\n  port: ${port}\n${auth}`;
    fs.writeFileSync(configPath, config(''));
    const hashed = spawnSync(binary, ['-config', configPath, 'hash-password'], {
        cwd: scratch, input: 'kula-session-test\n', encoding: 'utf8',
    });
    const hash = hashed.stdout.match(/password_hash: "([0-9a-f]+)"/)?.[1];
    const salt = hashed.stdout.match(/password_salt: "([0-9a-f]+)"/)?.[1];
    if (!hash || !salt) throw new Error(`hash-password failed:\n${hashed.stdout}${hashed.stderr}`);
    fs.writeFileSync(configPath, config(`  auth:\n    enabled: true\n    username: admin\n` +
        `    password_hash: "${hash}"\n    password_salt: "${salt}"\n`));

    const startServer = async () => {
        server = spawn(binary, ['-config', configPath, 'serve'], {
            cwd: scratch, stdio: ['ignore', 'pipe', 'pipe'],
        });
        serverClosed = new Promise(resolve => server.once('close', resolve));
        const capture = chunk => { serverOutput = (serverOutput + chunk).slice(-16384); };
        server.stdout.on('data', capture);
        server.stderr.on('data', capture);
        await waitFor(async () => {
            if (server.exitCode !== null) throw new Error(`kula serve exited early:\n${serverOutput}`);
            return fetch(`${base}/health`).then(response => response.ok, () => false);
        }, 'kula serve did not become healthy');
    };
    await startServer();

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
    // Chromium reports a refused handshake as a frame error, without its
    // status, so a socket that closes without a 101 counts as refused.
    const sockets = { created: 0, accepted: 0, refused: 0 };
    const upgraded = new Set();
    let heldProbes = 0;
    socket.addEventListener('message', event => {
        const message = JSON.parse(event.data);
        if (pending.has(message.id)) {
            const { resolve, reject } = pending.get(message.id);
            pending.delete(message.id);
            if (message.error) reject(new Error(JSON.stringify(message.error)));
            else resolve(message.result);
        } else if (message.method === 'Network.webSocketCreated') {
            sockets.created++;
        } else if (message.method === 'Network.webSocketHandshakeResponseReceived' &&
            message.params.response.status === 101) {
            sockets.accepted++;
            upgraded.add(message.params.requestId);
        } else if (message.method === 'Network.webSocketClosed' && !upgraded.delete(message.params.requestId)) {
            sockets.refused++;
        } else if (message.method === 'Fetch.requestPaused') {
            heldProbes++; // never continued: a probe on a network that drops packets
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
    await call('Network.enable');
    await call('Runtime.enable');
    await watchCSPViolations(call);
    const evaluate = async expression => {
        const out = await call('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
        if (out.exceptionDetails) throw new Error(JSON.stringify(out.exceptionDetails));
        return out.result.value;
    };
    const loginShown = () => evaluate(`(() => {
        const overlay = document.getElementById('login-overlay');
        return !!overlay && !overlay.classList.contains('hidden');
    })()`);
    const signIn = async () => {
        const accepted = sockets.accepted;
        await evaluate(`(() => {
            document.getElementById('login-user').value = 'admin';
            document.getElementById('login-pass').value = 'kula-session-test';
            document.getElementById('login-form').requestSubmit();
        })()`);
        await waitFor(async () => !(await loginShown()) && sockets.accepted > accepted,
        'signing in did not open the live stream');
        // The overlay only hides, so the password must be cleared from it.
        assert.equal(await evaluate(`document.getElementById('login-pass').value`), '',
            'the password stayed in the hidden login form after signing in');
    };

    await call('Page.navigate', { url: `${base}/` });
    await waitFor(loginShown, 'the dashboard did not ask to sign in');
    // A wrong password is reported as such, in the page's language (a rate
    // limit or a busy server has a message of its own).
    await evaluate(`(() => {
        document.getElementById('login-user').value = 'admin';
        document.getElementById('login-pass').value = 'not-the-password';
        document.getElementById('login-form').requestSubmit();
    })()`);
    await waitFor(() => evaluate(`(() => {
        const error = document.getElementById('login-error');
        return !error.classList.contains('hidden') && error.textContent === 'Invalid username or password';
    })()`), 'a wrong password did not show the invalid-credentials message');
    await signIn();

    // A restart without sessions.json forgets the session the page holds.
    await stopProcess(server, serverClosed);
    const sessions = path.join(data, 'sessions.json');
    assert.ok(fs.existsSync(sessions), 'shutdown did not save sessions.json');
    fs.rmSync(sessions);
    const refusedBefore = sockets.refused;
    await startServer();

    await waitFor(loginShown, 'the dashboard kept reconnecting with an expired session', 30000);
    assert.ok(sockets.refused > refusedBefore, `no reconnect was refused: ${JSON.stringify(sockets)}`);
    // Once the login screen is up, nothing retries behind it.
    const settled = sockets.created;
    await delay(4000);
    assert.equal(sockets.created, settled, `reconnects continued behind the login screen: ${JSON.stringify(sockets)}`);

    await signIn();

    // A refused handshake asks /api/auth/status before retrying. Where packets
    // are dropped that request hangs, held here, and the page must give up on
    // it and keep retrying rather than wait for the browser's connect timeout.
    await call('Fetch.enable', { patterns: [{ urlPattern: '*/api/auth/status*' }] });
    await stopProcess(server, serverClosed);
    await waitFor(() => heldProbes > 0, 'a refused reconnect did not ask whether the session holds');
    const probing = sockets.created;
    await waitFor(() => sockets.created > probing, 'a hung session probe stopped the reconnects');
    await call('Fetch.disable');
    const acceptedBefore = sockets.accepted;
    await startServer();
    await waitFor(async () => sockets.accepted > acceptedBefore && !(await loginShown()),
        'the dashboard did not stream again once the server was back', 30000);

    assert.deepEqual(errors, [], 'the dashboard threw exceptions');
    assert.deepEqual(cspViolations, [], 'the page broke its Content-Security-Policy');
    console.log(JSON.stringify({ status: 'pass', sockets, heldProbes }));
} catch (error) {
    if (serverOutput) console.error(`kula serve output:\n${serverOutput}`);
    if (errors.length) console.error(`dashboard exceptions:\n${errors.join('\n')}`);
    if (cspViolations.length) console.error(`CSP violations:\n${cspViolations.join('\n')}`);
    throw error;
} finally {
    socket?.close();
    await stopProcess(browser, browserClosed);
    await stopProcess(server, serverClosed);
    fs.rmSync(scratch, { recursive: true, force: true });
}
