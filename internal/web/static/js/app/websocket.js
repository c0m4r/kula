/* ============================================================
   websocket.js — WebSocket connection, reconnect logic,
   and live queue drain.
   ============================================================ */
'use strict';
import { state } from './state.js';
import { pushLiveSample, fetchHistory, fetchGapHistory } from './charts-data.js';

export function connectWS() {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const wsUrl = `${proto}//${location.host}/ws`;

    try {
        state.ws = new WebSocket(wsUrl);
    } catch (e) {
        scheduleReconnect();
        return;
    }

    state.ws.onopen = () => {
        state.connected = true;
        state.reconnectDelay = 1000;
        updateConnectionStatus(true);
        // Load history for the current time window on first connect
        if (!state.historyLoaded) {
            state.historyLoaded = true;
            fetchHistory(state.timeRange);
        } else if (state.lastHistoricalTs) {
            fetchGapHistory(state.lastHistoricalTs, new Date());
        }
    };

    state.ws.onmessage = (evt) => {
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

    state.ws.onclose = () => {
        state.connected = false;
        updateConnectionStatus(false);
        scheduleReconnect();
    };

    state.ws.onerror = () => {
        state.ws.close();
    };
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
