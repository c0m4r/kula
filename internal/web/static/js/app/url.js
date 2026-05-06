/* ============================================================
   url.js — Base-path-aware URL helpers.
   All API fetches and WebSocket connections must go through
   these helpers so Kula works correctly behind a reverse proxy
   sub-path (web.base_path config option).
   When base_path is "" the helpers are transparent no-ops.
   ============================================================ */
'use strict';

export const basePath = document.querySelector('meta[name="kula-base"]')?.content ?? '';

export const apiUrl = path => basePath + path;

export function connectWsUrl() {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    return `${proto}//${location.host}${basePath}/ws`;
}
