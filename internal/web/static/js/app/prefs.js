/* ============================================================
   prefs.js — Dashboard preferences in browser storage.
   localStorage can be missing or throw on every access (blocked site data,
   some private modes, sandboxed frames) or refuse a write (a full quota).
   Preferences then last for this page only instead of stopping the
   dashboard. Every module reads and writes preferences through here.
   ============================================================ */
'use strict';

// Values whose write failed, so this page still sees what it last stored.
const unsaved = new Map();

/** The stored string for key, or null when there is none or storage is blocked. */
export function readPref(key) {
    if (unsaved.has(key)) return unsaved.get(key);
    try {
        return localStorage.getItem(key);
    } catch (_error) {
        return null;
    }
}

/** The stored JSON value for key, or fallback when it is missing or not JSON. */
export function readJsonPref(key, fallback) {
    const text = readPref(key);
    if (text === null || text === '') return fallback;
    try {
        return JSON.parse(text);
    } catch (_error) {
        return fallback;
    }
}

export function writePref(key, value) {
    const text = String(value);
    try {
        localStorage.setItem(key, text);
        unsaved.delete(key);
    } catch (_error) {
        unsaved.set(key, text);
    }
}

export function writeJsonPref(key, value) {
    writePref(key, JSON.stringify(value));
}

export function removePref(key) {
    unsaved.delete(key);
    try {
        localStorage.removeItem(key);
    } catch (_error) { /* storage blocked: nothing was stored */ }
}
