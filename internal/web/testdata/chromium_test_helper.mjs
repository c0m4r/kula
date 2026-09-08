import fs from 'node:fs';
import path from 'node:path';
import { spawn } from 'node:child_process';

export const delay = ms => new Promise(resolve => setTimeout(resolve, ms));

export function findChromium() {
    return process.env.KULA_CHROMIUM || [
        '/usr/bin/chromium',
        '/usr/bin/chromium-browser',
        '/usr/bin/google-chrome',
        '/usr/bin/google-chrome-stable',
    ].find(candidate => fs.existsSync(candidate));
}

function devToolsEndpoint(userDataDir, output) {
    const logged = output.match(/DevTools listening on (ws:\/\/[^\s]+)/)?.[1];
    if (logged) return logged;

    try {
        const [port, target] = fs.readFileSync(path.join(userDataDir, 'DevToolsActivePort'), 'utf8')
            .trim().split(/\r?\n/);
        if (!/^\d+$/.test(port) || !target) return undefined;
        if (target.startsWith('ws://') || target.startsWith('wss://')) return target;
        if (!target.startsWith('/')) return undefined;
        return `ws://127.0.0.1:${port}${target}`;
    } catch (error) {
        if (error.code === 'ENOENT') return undefined;
        throw error;
    }
}

function launchError(message, browserPath, browser, output) {
    const status = browser.exitCode !== null
        ? `exit code ${browser.exitCode}`
        : browser.signalCode !== null
            ? `signal ${browser.signalCode}`
            : 'still running';
    const details = output.trim() || 'Chromium produced no output.';
    return new Error(`${message}\nExecutable: ${browserPath}\nStatus: ${status}\n${details}`);
}

export async function launchChromium(browserPath, args, userDataDir, timeoutMs = 30000) {
    const browser = spawn(browserPath, args, { stdio: ['ignore', 'pipe', 'pipe'] });
    const browserClosed = new Promise(resolve => browser.once('close', resolve));
    let output = '';
    const capture = chunk => {
        output = (output + chunk).slice(-65536);
    };
    browser.on('error', capture);
    browser.stdout.on('data', capture);
    browser.stderr.on('data', capture);

    try {
        const deadline = Date.now() + timeoutMs;
        while (Date.now() < deadline) {
            if (browser.exitCode !== null || browser.signalCode !== null) {
                throw launchError('Chromium exited before exposing a DevTools endpoint.',
                    browserPath, browser, output);
            }
            const endpoint = devToolsEndpoint(userDataDir, output);
            if (endpoint) return { browser, browserClosed, endpoint };
            await delay(50);
        }
        throw launchError(`Chromium did not expose a DevTools endpoint within ${timeoutMs / 1000}s.`,
            browserPath, browser, output);
    } catch (error) {
        if (browser.exitCode === null && browser.signalCode === null) browser.kill('SIGTERM');
        await browserClosed;
        throw error;
    }
}
