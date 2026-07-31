/* ============================================================
   section-utils.js — Resolve section title ↔ charts-grid pairs
   even when the title sits inside a .section-head wrapper
   (Applications header + filter + shared legend).
   ============================================================ */
'use strict';

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
        t.classList.remove('focus-active', 'focus-selecting', 'focus-hidden');
    });
    document.querySelectorAll('.section-head').forEach(h => {
        h.classList.remove('focus-active', 'focus-selecting', 'focus-hidden');
    });
}
