/* ============================================================
   focus-mode.js — Focus mode: select and persist a subset of
   chart cards to display.
   ============================================================ */
'use strict';
import { state } from './state.js';
import { readPref, removePref, writeJsonPref, writePref } from './prefs.js';
import { i18n } from './i18n.js';
import { setSectionFocusChrome, clearAllSectionFocusChrome } from './section-utils.js';
// The Applications header carries the shared container colour legend, which is
// the only labelling for the multi-series container charts.
import { syncApplicationsHeaderFocus } from './container-apps.js';
import { enterTvMode, exitTvMode, isTvModeStored } from './tv-mode.js';
import { arrangeCards, chartCards, registerCardDecorator } from './card-layout.js';

// Container metrics moved from "one card per container × metric" to a fixed set
// of multi-series cards. Focus selections are persisted, so stored ids from the
// old layout must be mapped forward or the dashboard restores empty.
const LEGACY_CONTAINER_CARD_RE = /^card-container_.+_(cpu|mem|io)$/;
const LEGACY_CONTAINER_CARD_MAP = {
    cpu: ['card-containers-cpu'],
    mem: ['card-containers-mem'],
    // The combined I/O card became four direction-specific cards.
    io: [
        'card-containers-net-rx', 'card-containers-net-tx',
        'card-containers-disk-r', 'card-containers-disk-w',
    ],
};

/**
 * Rewrite persisted focus ids from the pre-aggregation container layout.
 * Unrelated ids keep their order; expanded ids are deduplicated.
 */
function migrateLegacyFocusIds() {
    const stored = state.focusVisible;
    if (!Array.isArray(stored) || stored.length === 0) return;

    const out = [];
    const seen = new Set();
    const push = (id) => {
        if (seen.has(id)) return;
        seen.add(id);
        out.push(id);
    };

    let changed = false;
    for (const id of stored) {
        const match = LEGACY_CONTAINER_CARD_RE.exec(id);
        if (!match) {
            push(id);
            continue;
        }
        changed = true;
        LEGACY_CONTAINER_CARD_MAP[match[1]].forEach(push);
    }
    // Deduplication alone is also worth persisting.
    if (!changed && out.length === stored.length) return;

    state.focusVisible = out;
    writeJsonPref('kula_focus_visible', out);
}

// Run before any consumer reads state.focusVisible.
migrateLegacyFocusIds();

function attachFocusClick(card) {
    if (card._focusClick) return;
    card._focusClick = () => card.classList.toggle('focus-selected');
    card.addEventListener('click', card._focusClick);
}

// Cards created after Focus mode was applied (split, battery/UPS and
// application cards) join it the moment card-layout.js places them.
registerCardDecorator(card => {
    if (!card.classList.contains('chart-card')) return;
    const selected = !!state.focusVisible?.includes(card.id);
    if (state.focusSelecting) {
        card.classList.toggle('focus-selected', selected);
        attachFocusClick(card);
    } else if (state.focusMode) {
        card.classList.toggle('focus-visible', selected);
    }
});

export function toggleFocusMode() {
    // Rearranging owns the cards until it is finished.
    if (state.rearranging) return;
    const grids = document.querySelectorAll('.charts-grid');
    const btn = document.getElementById('btn-focus');

    if (state.focusMode && !state.focusSelecting) {
        // Exit focus mode
        exitTvMode();
        state.focusMode = false;
        grids.forEach(g => g.classList.remove('focus-active', 'focus-selecting', 'focus-hidden'));
        clearAllSectionFocusChrome();
        btn.classList.remove('focus-active');
        document.getElementById('gauges-row')?.classList.remove('focus-hidden');
        document.getElementById('chart-search')?.classList.remove('focus-hidden');
        chartCards().forEach(card => card.classList.remove('focus-visible', 'focus-selected'));
        removeFocusBar();
        removePref('kula_focus_visible');
        state.focusVisible = null;
        restoreGrids();
        document.dispatchEvent(new Event('kula-history-sections-changed'));
        return;
    }

    if (state.focusSelecting) {
        // Apply selection
        const tvRequested = document.getElementById('focus-tv-mode-chk')?.checked === true;
        const selected = chartCards()
            .filter(card => card.classList.contains('focus-selected'))
            .map(card => card.id);

        if (selected.length === 0) {
            // No selection = exit
            state.focusMode = false;
            state.focusSelecting = false;
            grids.forEach(g => g.classList.remove('focus-active', 'focus-selecting'));
            clearAllSectionFocusChrome();
            btn.classList.remove('focus-active');
            removeFocusBar();
            removePref('kula_focus_visible');
            state.focusVisible = null;
            restoreGrids();
            document.dispatchEvent(new Event('kula-history-sections-changed'));
            return;
        }

        state.focusVisible = selected;
        writeJsonPref('kula_focus_visible', selected);
        state.focusSelecting = false;
        chartCards().forEach(card => card.classList.remove('focus-selected'));

        applyFocusLayout();
        removeFocusBar();
        if (tvRequested) enterTvMode();
        else exitTvMode();
        document.dispatchEvent(new Event('kula-history-sections-changed'));
        return;
    }

    // Enter selection mode
    state.focusMode = true;
    state.focusSelecting = true;
    grids.forEach(g => {
        g.classList.add('focus-selecting');
        g.classList.remove('focus-active', 'focus-hidden');
    });
    document.querySelectorAll('.section-title').forEach(t => {
        setSectionFocusChrome(t, { active: false, selecting: true, hidden: false });
    });
    btn.classList.add('focus-active');
    // First: showing the bar removes it and the card click handlers of any
    // earlier selection, so it would also detach the ones attached below.
    showFocusBar();

    // Pre-select previously visible cards; a click toggles a card.
    const visible = new Set(state.focusVisible || []);
    chartCards().forEach(card => {
        card.classList.toggle('focus-selected', visible.has(card.id));
        attachFocusClick(card);
    });
}

// The bar is built after the translations were applied to the page, so it
// translates its own labels; data-i18n keeps them following language changes.
function translatedLabel(element, key, fallback) {
    element.setAttribute('data-i18n', key);
    element.textContent = i18n.translations[key] || fallback;
}

function focusBarCheckbox(id, key, fallback, checked) {
    const label = document.createElement('label');
    label.className = 'focus-bar-checkbox';

    const chk = document.createElement('input');
    chk.type = 'checkbox';
    chk.id = id;
    chk.checked = checked;

    const text = document.createElement('span');
    translatedLabel(text, key, fallback);

    label.appendChild(chk);
    label.appendChild(text);
    return { label, chk };
}

export function showFocusBar() {
    removeFocusBar();
    const bar = document.createElement('div');
    bar.className = 'focus-bar';
    bar.id = 'focus-bar';
    const hideGauges = readPref('kula_focus_hide_gauges') === 'true';
    
    const spanWrapper = document.createElement('span');
    const spanText = document.createElement('span');
    translatedLabel(spanText, 'select_graphs', 'Select graphs to display, then click Done');
    spanWrapper.appendChild(spanText);

    const gauges = focusBarCheckbox('focus-hide-gauges-chk', 'hide_gauges', 'Hide gauges', hideGauges);
    // Applied with Done, unlike Hide gauges: TV mode takes over the screen.
    const tv = focusBarCheckbox('focus-tv-mode-chk', 'tv_mode', 'TV mode', isTvModeStored());
    tv.label.setAttribute('data-i18n-title', 'tv_mode_hint');
    tv.label.title = i18n.translations.tv_mode_hint ||
        'Fill the screen with the selected charts, without dashboard controls';

    const btnDone = document.createElement('button');
    btnDone.id = 'btn-focus-done';
    translatedLabel(btnDone, 'done', 'Done');

    const btnCancel = document.createElement('button');
    btnCancel.id = 'btn-focus-cancel';
    translatedLabel(btnCancel, 'cancel', 'Cancel');

    bar.appendChild(spanWrapper);
    bar.appendChild(gauges.label);
    bar.appendChild(tv.label);
    bar.appendChild(btnDone);
    bar.appendChild(btnCancel);

    const firstGrid = document.querySelector('.charts-grid');
    if (firstGrid) firstGrid.parentNode.insertBefore(bar, firstGrid);

    gauges.chk.addEventListener('change', (e) => {
        writePref('kula_focus_hide_gauges', e.target.checked ? 'true' : 'false');
    });

    btnDone.addEventListener('click', toggleFocusMode);
    btnCancel.addEventListener('click', () => {
        state.focusSelecting = false;
        state.focusMode = false;
        document.querySelectorAll('.charts-grid').forEach(g => g.classList.remove('focus-selecting', 'focus-hidden'));
        clearAllSectionFocusChrome();
        document.getElementById('btn-focus').classList.remove('focus-active');
        document.getElementById('gauges-row')?.classList.remove('focus-hidden');
        document.getElementById('chart-search')?.classList.remove('focus-hidden');
        chartCards().forEach(card => card.classList.remove('focus-selected'));
        removeFocusBar();
        restoreGrids();
    });
}

export function removeFocusBar() {
    const bar = document.getElementById('focus-bar');
    if (bar) bar.remove();
    chartCards().forEach(card => {
        if (card._focusClick) {
            card.removeEventListener('click', card._focusClick);
            delete card._focusClick;
        }
    });
}

// Show the stored selection: every section's cards in #charts-grid (in their
// sections' order, see card-layout.js), only the selected ones visible.
// Hidden cards keep focus-visible; the stylesheet shows them once they are
// no longer hidden, so no per-sample pass is needed.
function applyFocusLayout() {
    const selected = new Set(state.focusVisible || []);
    const mainGrid = document.getElementById('charts-grid');
    document.querySelectorAll('.charts-grid').forEach(grid => {
        grid.classList.remove('focus-selecting');
        grid.classList.toggle('focus-active', grid === mainGrid);
        grid.classList.toggle('focus-hidden', grid !== mainGrid);
    });
    document.querySelectorAll('.section-title').forEach(t => {
        setSectionFocusChrome(t, { active: false, selecting: false, hidden: true });
    });
    chartCards().forEach(card => card.classList.toggle('focus-visible', selected.has(card.id)));

    if (readPref('kula_focus_hide_gauges') === 'true') {
        document.getElementById('gauges-row')?.classList.add('focus-hidden');
    }
    document.getElementById('chart-search')?.classList.add('focus-hidden');

    arrangeCards();
    // Container cards just left the Applications grid; keep their legend with them.
    syncApplicationsHeaderFocus();
}

export function applyStoredFocusMode() {
    if (state.focusVisible && state.focusVisible.length > 0) {
        state.focusMode = true;
        document.getElementById('btn-focus')?.classList.add('focus-active');
        applyFocusLayout();
    }
}

// Return every card to its own section, in that section's order.
function restoreGrids() {
    arrangeCards();
    syncApplicationsHeaderFocus();
}
