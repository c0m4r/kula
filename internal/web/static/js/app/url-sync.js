/* ============================================================
   url-sync.js — Encode/decode the current time viewport in the
   URL so it can be shared. Uses ?from=<ms>&to=<ms>.
   ============================================================ */
'use strict';

export function updateUrlState(fromMs, toMs) {
    const url = new URL(window.location.href);
    url.searchParams.set('from', Math.round(fromMs));
    url.searchParams.set('to', Math.round(toMs));
    history.replaceState(null, '', url);
}

export function clearUrlState() {
    const url = new URL(window.location.href);
    if (url.searchParams.has('from') || url.searchParams.has('to')) {
        url.searchParams.delete('from');
        url.searchParams.delete('to');
        history.replaceState(null, '', url);
    }
}

// Returns { from: Date, to: Date } if valid params are present, otherwise null.
export function readUrlState() {
    const params = new URLSearchParams(window.location.search);
    const fromRaw = params.get('from');
    const toRaw = params.get('to');
    if (!fromRaw || !toRaw) return null;
    const fromMs = parseInt(fromRaw, 10);
    const toMs = parseInt(toRaw, 10);
    if (isNaN(fromMs) || isNaN(toMs) || fromMs >= toMs) return null;
    return { from: new Date(fromMs), to: new Date(toMs) };
}
