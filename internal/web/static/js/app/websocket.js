/* ============================================================
   websocket.js — WebSocket connection, reconnect logic,
   and live queue drain.
   ============================================================ */
'use strict';
import { state } from './state.js';
import { pushLiveSample, fetchHistory, fetchCustomHistory, fetchGapHistory } from './charts-data.js';
import { wsUrl } from './api.js';

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
        updateConnectionStatus(true);
        // Load history for the current time window on first connect.
        // The window may be a preset range or a custom range restored
        // from the URL (see url-state.js).
        if (!state.historyLoaded) {
            state.historyLoaded = true;
            if (state.timeRange !== null) {
                fetchHistory(state.timeRange);
            } else if (state.customFrom && state.customTo) {
                fetchCustomHistory(state.customFrom, state.customTo);
            }
        } else if (state.lastHistoricalTs) {
            fetchGapHistory(state.lastHistoricalTs, new Date());
        }
    };

    ws.onmessage = (evt) => {
        if (state.ws !== ws) return;
        if (evt.data.length > 1024 * 1024) { // 1MB limit
            console.error('WebSocket message too large');
            return;
        }
        if (state.loadingHistory) {
            // Buffer samples that arrive while history is loading so there
            // is no gap when live streaming resumes after the fetch.
            try {
                const sample = JSON.parse(evt.data);
                state.liveQueue.push(sample);
                if (state.liveQueue.length > 120) state.liveQueue.shift(); // cap at 2 min
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
