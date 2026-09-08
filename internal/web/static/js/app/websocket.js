/* ============================================================
   websocket.js — WebSocket connection, reconnect logic,
   and live queue drain.
   ============================================================ */
'use strict';
import { state } from './state.js';
import { pushLiveSample, fetchHistory, fetchCustomHistory } from './charts-data.js';
import { wsUrl } from './api.js';
import { normalizeHistoryItem } from './history-data.js';

export function connectWS() {
    if (state.ws && (state.ws.readyState === WebSocket.CONNECTING || state.ws.readyState === WebSocket.OPEN)) {
        return;
    }

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

    ws.onclose = () => {
        // Ignore a close from a socket that disconnectWS deliberately detached,
        // or that has since been replaced by a newer connection.
        if (state.ws !== ws) return;
        state.ws = null;
        state.connected = false;
        updateConnectionStatus(false);
        scheduleReconnect();
    };

    ws.onerror = () => {
        ws.close();
    };
}

export function disconnectWS() {
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
    state.reconnectTimer = setTimeout(() => {
        state.reconnectTimer = null;
        connectWS();
    }, state.reconnectDelay);
    state.reconnectDelay = Math.min(state.reconnectDelay * 1.5, 30000);
}

export function updateConnectionStatus(connected) {
    const dot = document.getElementById('connection-status');
    if (dot) {
        dot.className = 'status-dot ' + (connected ? 'connected' : 'disconnected');
        dot.title = connected ? 'Connected' : 'Disconnected';
    }
}
