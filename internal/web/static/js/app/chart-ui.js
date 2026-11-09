/* ============================================================
   chart-ui.js — Commit chart presentation once per history batch.
   Dataset ingestion remains synchronous and retains every sample.
   ============================================================ */
'use strict';

let pending = null;

export function batchChartUI(ingest) {
    if (pending) return ingest();
    const updates = new Map();
    pending = updates;
    try {
        ingest();
    } finally {
        pending = null;
    }
    for (const render of updates.values()) render();
}

export function updateChartUI(key, render) {
    if (!pending) {
        render();
        return;
    }
    // Preserve the order of the last writes: section visibility can depend
    // on card visibility, and focus layout can depend on both.
    pending.delete(key);
    pending.set(key, render);
}

export function setChartHidden(id, hidden) {
    updateChartUI(`hidden:${id}`, () => {
        const element = document.getElementById(id);
        if (element && element.classList.contains('hidden') !== hidden) {
            element.classList.toggle('hidden', hidden);
        }
    });
}

// The last formatter of each subtitle, so a language change can rewrite the
// subtitles of a paused view or of cards whose data stopped arriving.
const subtitleFormats = new Map();

function renderSubtitle(id, formatText) {
    const element = document.getElementById(id);
    if (!element) return false;
    const text = formatText();
    if (element.textContent !== text) element.textContent = text;
    return true;
}

export function setChartSubtitle(id, formatText) {
    subtitleFormats.set(id, formatText);
    updateChartUI(`subtitle:${id}`, () => renderSubtitle(id, formatText));
}

// Re-run every subtitle's last formatter, forgetting those whose card is gone.
export function refreshChartSubtitles() {
    for (const [id, formatText] of subtitleFormats) {
        if (!renderSubtitle(id, formatText)) subtitleFormats.delete(id);
    }
}
