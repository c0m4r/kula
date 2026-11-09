/* ============================================================
   websocket.js — WebSocket connection and reconnect logic;
   queues live samples during a history load (charts-data.js
   drains them).
   ============================================================ */
'use strict';
import { state } from './state.js';
import { pushLiveSample, fetchHistory, fetchCustomHistory } from './charts-data.js';
import { apiUrl, wsUrl } from './api.js';
import { normalizeHistoryItem } from './history-data.js';
import { requireLogin } from './auth.js';
import { i18n } from './i18n.js';

// Bumped by every new connection and by disconnectWS, so a session check
// still in flight for an older socket cannot act after a login or logout.
let connectGeneration = 0;

export function connectWS() {
    if (state.ws && (state.ws.readyState === WebSocket.CONNECTING || state.ws.readyState === WebSocket.OPEN)) {
        return;
    }

    const generation = ++connectGeneration;
    let opened = false;
    let ws;
    try {
        ws = new WebSocket(wsUrl('/ws'));
        state.ws = ws;
    } catch (e) {
        scheduleReconnect();
        return;
    }

    ws.onopen = () => {
        if (state.ws !== ws) {
            ws.close(1000, 'superseded');
            return;
        }
        opened = true;
        state.connected = true;
        state.reconnectDelay = 1000;
        state.liveSampleIntervalMs = null;
        state.liveSampleIntervals = [];
        state.lastLiveSampleTs = null;
        updateConnectionStatus(true);
        // Load history for the current time window on first connect.
        // The window may be a preset range or a custom range restored
        // from the URL (see history-navigation.js).
        if (!state.historyLoaded) {
            state.historyLoaded = true;
            if (state.timeRange !== null) {
                fetchHistory(state.timeRange);
            } else if (state.customFrom && state.customTo) {
                fetchCustomHistory(state.customFrom, state.customTo);
            }
        } else if (state.timeRange !== null && state.lastHistoricalTs && !state.loadingHistory) {
            // A reconnect repair is lower priority than an explicit viewport
            // request already in flight. Exact historical intervals are
            // frozen and must never be extended by reconnect repair.
            fetchHistory(state.timeRange, { background: true });
        }
    };

    ws.onmessage = (evt) => {
        if (state.ws !== ws) return;
        if (evt.data.length > 1024 * 1024) { // 1MB limit
            console.error('WebSocket message too large');
            return;
        }
        if (state.loadingHistory && state.queueLiveDuringHistory) {
            // Foreground loads replace the current view, so buffer samples
            // until that replacement settles. Background snapshot refreshes
            // still pass through pushLiveSample to keep gauges and alerts live.
            try {
                const item = normalizeHistoryItem(JSON.parse(evt.data));
                state.liveQueue.push(item);
                const interval = state.liveSampleIntervalMs || 1000;
                const queueLimit = Math.min(state.maxBufferSize, Math.max(120, Math.ceil(120000 / interval)));
                if (state.liveQueue.length > queueLimit) state.liveQueue.shift();
            } catch (e) { /* ignore */ }
            return;
        }
        try {
            const sample = JSON.parse(evt.data);
            pushLiveSample(sample);
        } catch (e) {
            console.error('Parse error:', e);
        }
    };

    ws.onclose = event => {
        // Ignore a close from a socket that disconnectWS deliberately detached,
        // or that has since been replaced by a newer connection.
        if (state.ws !== ws) return;
        state.ws = null;
        state.connected = false;
        updateConnectionStatus(false);
        if (event.code === 1008) {
            // The server rechecks sessions during streaming and uses 1008 for
            // expiry/revocation. Reconnecting cannot renew those credentials.
            requireLogin();
            return;
        }
        if (!opened) {
            recheckSessionThenReconnect(generation);
            return;
        }
        scheduleReconnect();
    };

    ws.onerror = () => {
        ws.close();
    };
}

// SESSION_PROBE_TIMEOUT_MS bounds the session probe below. Where packets are
// dropped (host unreachable, a VPN flap) a request can wait for the browser's
// connect timeout, minutes, and the page would not retry in the meantime.
const SESSION_PROBE_TIMEOUT_MS = 5000;

// A refused handshake -- a 401 once the session expired while the page slept,
// a 429 at the connection limit -- reaches the page only as a close before
// open (code 1006), without its status. Ask the server whether the session
// still holds: retrying cannot renew one that has expired.
function recheckSessionThenReconnect(generation) {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), SESSION_PROBE_TIMEOUT_MS);
    fetch(apiUrl('/api/auth/status'), { signal: controller.signal })
        .then(r => r.json())
        .then(data => Boolean(data?.auth_required && !data.authenticated))
        .catch(() => false)
        .then(expired => {
            clearTimeout(timeout);
            if (generation !== connectGeneration || state.ws) return;
            if (expired) requireLogin();
            else scheduleReconnect();
        });
}

export function disconnectWS() {
    connectGeneration++;
    if (state.reconnectTimer) {
        clearTimeout(state.reconnectTimer);
        state.reconnectTimer = null;
    }

    const ws = state.ws;
    state.ws = null;
    state.connected = false;
    state.reconnectDelay = 1000;
    state.historyLoaded = false;
    state.lastHistoricalTs = null;
    state.liveSampleIntervalMs = null;
    state.liveSampleIntervals = [];
    state.lastLiveSampleTs = null;
    updateConnectionStatus(false);

    if (ws && (ws.readyState === WebSocket.CONNECTING || ws.readyState === WebSocket.OPEN)) {
        ws.close(1000, 'logout');
    }
}


export function scheduleReconnect() {
    if (state.reconnectTimer) return;
    // Wait between half and all of the backoff, so dashboards a restart
    // disconnected together do not all reconnect in the same instant.
    const delay = state.reconnectDelay * (0.5 + Math.random() * 0.5);
    state.reconnectTimer = setTimeout(() => {
        state.reconnectTimer = null;
        connectWS();
    }, delay);
    state.reconnectDelay = Math.min(state.reconnectDelay * 1.5, 30000);
}

export function updateConnectionStatus(connected) {
    const dot = document.getElementById('connection-status');
    if (dot) {
        const status = connected ? 'connected' : 'disconnected';
        dot.className = 'status-dot ' + status;
        dot.dataset.i18nTitle = status;
        dot.title = i18n.t(status);
    }
}
