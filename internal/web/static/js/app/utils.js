/* ============================================================
   utils.js — Formatting utility functions.
   ============================================================ */
'use strict';

export function formatBytesShort(bytes) {
    if (bytes === 0 || bytes === undefined || bytes === null || isNaN(bytes)) return '0 B';
    if (Math.abs(bytes) < 1) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    const i = Math.floor(Math.log(Math.abs(bytes)) / Math.log(1024));
    const idx = Math.max(0, Math.min(i, units.length - 1));
    return (bytes / Math.pow(1024, idx)).toFixed(idx > 0 ? 1 : 0) + ' ' + units[idx];
}

export function formatMbps(v) {
    if (v < 1) return (v * 1000).toFixed(0) + ' Kbps';
    return v.toFixed(2) + ' Mbps';
}

export function formatPPS(v) {
    if (v === undefined || v === null || isNaN(v)) return '0 pps';
    if (v >= 1000000) return (v / 1000000).toFixed(1) + ' Mpps';
    if (v >= 1000) return (v / 1000).toFixed(1) + ' Kpps';
    return Math.round(v) + ' pps';
}
