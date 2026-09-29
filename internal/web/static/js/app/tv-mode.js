/* ============================================================
   tv-mode.js — TV mode: a Focus Mode presentation for wall
   displays. The selected charts fill the viewport without
   dashboard controls, the screen is kept awake, and the cursor
   hides while idle.
   Import-free so the grid heuristic loads directly in Node tests.
   ============================================================ */
'use strict';

const STORAGE_KEY = 'kula_focus_tv';
// Time axes read best wide: pick the grid whose cells fit the largest chart
// of this width:height ratio.
const CHART_ASPECT = 2;
const IDLE_MS = 3000;
const ACTIVITY_EVENTS = ['pointermove', 'pointerdown', 'keydown', 'wheel', 'touchstart'];

let active = false;
let layoutFrame = null;
let mutationObserver = null;
let resizeObserver = null;
let idleTimer = null;
let wakeLock = null;
let wakeLockPending = false;
let initialized = false;

/**
 * Choose columns × rows for `count` equally sized cells in a width × height
 * area separated by `gap`. The winning shape fits the widest chart of the
 * given aspect ratio; ties keep fewer columns so time axes stay wide.
 */
export function tvGridShape(count, width, height, gap = 0, aspect = CHART_ASPECT) {
    const cells = Math.max(1, Math.floor(count) || 1);
    let best = { columns: 1, rows: cells };
    if (!(width > 0) || !(height > 0)) return best;

    let bestWidth = -Infinity;
    for (let columns = 1; columns <= cells; columns++) {
        const rows = Math.ceil(cells / columns);
        const cellWidth = (width - gap * (columns - 1)) / columns;
        const cellHeight = (height - gap * (rows - 1)) / rows;
        const chartWidth = Math.min(cellWidth, cellHeight * aspect);
        if (chartWidth > bestWidth + 0.5) {
            best = { columns, rows };
            bestWidth = chartWidth;
        }
    }
    return best;
}

export function isTvModeActive() {
    return active;
}

export function isTvModeStored() {
    try {
        return localStorage.getItem(STORAGE_KEY) === 'true';
    } catch (_error) {
        return false;
    }
}

function storeTvMode(on) {
    try {
        if (on) localStorage.setItem(STORAGE_KEY, 'true');
        else localStorage.removeItem(STORAGE_KEY);
    } catch (_error) {
        // Without storage TV mode still works; it just is not restored.
    }
}

function chartsGrid() {
    return document.getElementById('charts-grid');
}

function scheduleLayout() {
    if (!active || layoutFrame !== null) return;
    layoutFrame = requestAnimationFrame(layoutGrid);
}

// Size the combined Focus Mode grid so every shown card fits on screen.
function layoutGrid() {
    layoutFrame = null;
    const grid = chartsGrid();
    if (!active || !grid) return;

    // Read all layout before writing, so the frame lays out only once.
    const cards = Array.from(grid.children).filter(el =>
        el.classList.contains('chart-card') && el.getClientRects().length > 0);
    const style = getComputedStyle(grid);
    const width = grid.clientWidth - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight);
    const height = grid.clientHeight - parseFloat(style.paddingTop) - parseFloat(style.paddingBottom);
    const { columns, rows } = tvGridShape(cards.length, width, height, parseFloat(style.rowGap) || 0);

    grid.style.setProperty('--tv-columns', String(columns));
    grid.style.setProperty('--tv-rows', String(rows));
    // The last card absorbs the empty cells of a partial final row.
    const span = columns * rows - cards.length + 1;
    const last = span > 1 ? cards[cards.length - 1] : null;
    grid.querySelectorAll(':scope > .tv-fill').forEach(card => {
        if (card !== last) card.classList.remove('tv-fill');
    });
    if (last) {
        // classList.add() rewrites the attribute even when the class is
        // present, which would re-trigger the observer every frame.
        if (!last.classList.contains('tv-fill')) last.classList.add('tv-fill');
        last.style.setProperty('--tv-span', String(span));
    }
}

function clearLayout() {
    const grid = chartsGrid();
    if (!grid) return;
    grid.style.removeProperty('--tv-columns');
    grid.style.removeProperty('--tv-rows');
    grid.querySelectorAll(':scope > .chart-card').forEach(card => {
        card.classList.remove('tv-fill');
        card.style.removeProperty('--tv-span');
    });
}

function observeGrid() {
    const grid = chartsGrid();
    if (!grid) return;
    // Cards join, leave, hide and unhide as telemetry arrives. Changes inside
    // a card (subtitles rewrite every second) do not affect the grid.
    mutationObserver = new MutationObserver(records => {
        if (records.some(record => record.target === grid || record.target.parentElement === grid)) {
            scheduleLayout();
        }
    });
    mutationObserver.observe(grid, { childList: true, subtree: true, attributes: true, attributeFilter: ['class'] });
    // Window size, the gauge row and text size all change the space available.
    if (typeof ResizeObserver === 'function') {
        resizeObserver = new ResizeObserver(scheduleLayout);
        resizeObserver.observe(grid);
    }
}

function tvControls() {
    return document.getElementById('tv-controls');
}

function goIdle() {
    idleTimer = null;
    const controls = tvControls();
    // Keep the controls while they are hovered or hold keyboard focus.
    if (controls?.matches(':hover') || controls?.querySelector(':focus-visible')) {
        idleTimer = setTimeout(goIdle, IDLE_MS);
        return;
    }
    document.documentElement.classList.add('tv-idle');
}

function onActivity() {
    document.documentElement.classList.remove('tv-idle');
    clearTimeout(idleTimer);
    idleTimer = setTimeout(goIdle, IDLE_MS);
}

async function requestWakeLock() {
    if (!active || wakeLock || wakeLockPending || document.visibilityState !== 'visible' ||
        typeof navigator.wakeLock?.request !== 'function') {
        return;
    }
    wakeLockPending = true;
    try {
        const lock = await navigator.wakeLock.request('screen');
        if (!active) {
            lock.release().catch(() => {});
            return;
        }
        wakeLock = lock;
        lock.addEventListener('release', () => {
            if (wakeLock === lock) wakeLock = null;
        });
    } catch (_error) {
        // Denied, or not a secure context: the display may sleep on its own schedule.
    } finally {
        wakeLockPending = false;
    }
}

function releaseWakeLock() {
    const lock = wakeLock;
    wakeLock = null;
    lock?.release().catch(() => {});
}

// The browser drops the wake lock whenever the page is hidden.
function onVisibilityChange() {
    if (document.visibilityState === 'visible') requestWakeLock();
}

function onKeyDown(event) {
    // A chart canvas consumes Escape first while it has a pinned point.
    if (!active || event.key !== 'Escape' || event.defaultPrevented) return;
    exitTvMode();
}

function syncFullscreenButton() {
    const button = document.getElementById('btn-tv-fullscreen');
    if (!button) return;
    button.hidden = !document.fullscreenEnabled;
    button.setAttribute('aria-pressed', String(!!document.fullscreenElement));
}

function toggleFullscreen() {
    if (document.fullscreenElement) {
        document.exitFullscreen().catch(() => {});
    } else {
        document.documentElement.requestFullscreen?.().catch(() => {});
    }
}

export function enterTvMode() {
    storeTvMode(true);
    if (active) {
        scheduleLayout();
        return;
    }
    active = true;
    document.documentElement.classList.add('tv-mode');
    observeGrid();
    ACTIVITY_EVENTS.forEach(type =>
        document.addEventListener(type, onActivity, { capture: true, passive: true }));
    document.addEventListener('visibilitychange', onVisibilityChange);
    syncFullscreenButton();
    // Show the controls briefly so the way out is discoverable.
    onActivity();
    requestWakeLock();
    // Hover no longer pauses live updates; see syncPauseState().
    document.dispatchEvent(new Event('kula-sync-pause'));
    scheduleLayout();
}

export function exitTvMode() {
    storeTvMode(false);
    if (!active) return;
    active = false;
    document.documentElement.classList.remove('tv-mode', 'tv-idle');
    mutationObserver?.disconnect();
    resizeObserver?.disconnect();
    mutationObserver = null;
    resizeObserver = null;
    if (layoutFrame !== null) cancelAnimationFrame(layoutFrame);
    layoutFrame = null;
    clearTimeout(idleTimer);
    idleTimer = null;
    ACTIVITY_EVENTS.forEach(type =>
        document.removeEventListener(type, onActivity, { capture: true }));
    document.removeEventListener('visibilitychange', onVisibilityChange);
    releaseWakeLock();
    clearLayout();
    if (document.fullscreenElement) document.exitFullscreen().catch(() => {});
    document.dispatchEvent(new Event('kula-sync-pause'));
}

/** Restore TV mode after a stored Focus Mode selection has been applied. */
export function applyStoredTvMode() {
    if (isTvModeStored()) enterTvMode();
}

export function initTvMode() {
    if (initialized) return;
    initialized = true;
    document.getElementById('btn-tv-exit')?.addEventListener('click', exitTvMode);
    document.getElementById('btn-tv-fullscreen')?.addEventListener('click', toggleFullscreen);
    document.addEventListener('fullscreenchange', syncFullscreenButton);
    document.addEventListener('keydown', onKeyDown);
    syncFullscreenButton();
}
