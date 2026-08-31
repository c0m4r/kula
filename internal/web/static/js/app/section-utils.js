/* ============================================================
   section-utils.js — Resolve section title ↔ charts-grid pairs
   even when the title sits inside a .section-head wrapper
   (Applications header + filter + shared legend).
   ============================================================ */
'use strict';

const FOCUS_CLASSES = ['focus-active', 'focus-selecting', 'focus-hidden'];

/** Section chrome element for a title: .section-head if present, else the title itself. */
export function sectionHeadForTitle(titleEl) {
    if (!titleEl) return null;
    return titleEl.closest('.section-head') || titleEl;
}

/**
 * Charts grid that belongs to a section title.
 * Walks past an optional .section-head wrapper, then finds the next .charts-grid.
 */
export function chartsGridForTitle(titleEl) {
    const start = sectionHeadForTitle(titleEl);
    if (!start) return null;
    let el = start.nextElementSibling;
    while (el && !el.classList.contains('charts-grid')) {
        el = el.nextElementSibling;
    }
    return el?.classList.contains('charts-grid') ? el : null;
}

/** Apply or clear focus-mode classes on both the title and its section head. */
export function setSectionFocusChrome(titleEl, { active = false, selecting = false, hidden = false } = {}) {
    if (!titleEl) return;
    titleEl.classList.toggle('focus-active', !!active);
    titleEl.classList.toggle('focus-selecting', !!selecting);
    titleEl.classList.toggle('focus-hidden', !!hidden);

    const head = titleEl.closest('.section-head');
    if (head) {
        head.classList.toggle('focus-active', !!active);
        head.classList.toggle('focus-selecting', !!selecting);
        head.classList.toggle('focus-hidden', !!hidden);
    }
}

/** Remove all focus-mode classes from every section title and section head. */
export function clearAllSectionFocusChrome() {
    document.querySelectorAll('.section-title').forEach(t => {
        t.classList.remove(...FOCUS_CLASSES);
    });
    document.querySelectorAll('.section-head').forEach(h => {
        h.classList.remove(...FOCUS_CLASSES, 'focus-pinned');
    });
}

/**
 * Keep a .section-head visible while any of the cards it describes is visible,
 * even after combineGrids() has moved those cards into the main grid.
 *
 * Focus mode relocates app cards out of their own grid, so the usual
 * "is my grid focus-active?" test hides the header — which for Applications
 * would take the shared colour legend with it, leaving the multi-series
 * container charts with no way to tell the series apart.
 */
export function pinSectionHeadForCards(headEl, cardSelector) {
    if (!headEl) return;
    const pinned = Array.from(document.querySelectorAll(cardSelector)).some(card =>
        card.classList.contains('focus-visible')
        && !card.classList.contains('hidden')
        && !card.classList.contains('chart-search-hidden')
    );
    headEl.classList.toggle('focus-pinned', pinned);
    if (pinned) headEl.classList.remove('focus-hidden');
}
