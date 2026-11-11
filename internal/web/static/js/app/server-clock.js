/* ============================================================
   server-clock.js — The server's clock, as live samples show it.
   ============================================================ */
'use strict';

// Live views end at "now" and samples carry the server's time. A browser
// clock behind the server's hid the newest samples past the right edge, and
// the history loaded up to the browser's now ended before the first live
// sample, leaving a hole; one ahead left an empty strip at the edge.
//
// Each sample's time minus its arrival is the offset less the delivery delay,
// so the largest of the last few is the closest estimate. Offsets within
// SKEW_DEAD_ZONE_MS (delivery, a synced clock's error) count as none, and a
// change smaller than that is ignored, so a synced clock behaves as before.
const SKEW_DEAD_ZONE_MS = 2000;
const SKEW_WINDOW = 10;

let offsetMs = 0;
const recent = [];

/** The server's current time, in milliseconds since the epoch. */
export function serverNow() {
    return Date.now() + offsetMs;
}

/** The applied offset of the server's clock from the browser's. */
export function serverClockOffset() {
    return offsetMs;
}

/**
 * Note a live sample stamped sampleMs that arrived at receivedMs. Returns true
 * when the applied offset changed, after which a view loaded up to the old
 * "now" should be fetched again.
 */
export function noteServerTime(sampleMs, receivedMs = Date.now()) {
    if (!Number.isFinite(sampleMs) || !Number.isFinite(receivedMs)) return false;
    recent.push(sampleMs - receivedMs);
    if (recent.length > SKEW_WINDOW) recent.shift();
    const estimate = Math.max(...recent);
    const next = Math.abs(estimate) < SKEW_DEAD_ZONE_MS ? 0 : Math.round(estimate);
    if (Math.abs(next - offsetMs) < SKEW_DEAD_ZONE_MS) return false;
    offsetMs = next;
    return true;
}

/** Forget every observation (tests). */
export function resetServerClock() {
    offsetMs = 0;
    recent.length = 0;
}
