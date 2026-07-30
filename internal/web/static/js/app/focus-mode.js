/* ============================================================
   focus-mode.js — Focus mode: select and persist a subset of
   chart cards to display.
   ============================================================ */
'use strict';
import { state } from './state.js';
import { i18n } from './i18n.js';

export const chartCardIds = [
    'card-cpu', 'card-loadavg', 'card-memory', 'card-swap',
    'card-network', 'card-pps', 'card-connections',
    'card-disk-io', 'card-disk-space',
    'card-gpu-load', 'card-vram',
    'card-processes', 'card-entropy', 'card-self',
    'card-cpu-temp', 'card-disk-temp', 'card-gpu-temp'
];

// Returns all currently visible split chart card elements
function _getSplitCards() {
    return Array.from(document.querySelectorAll('.chart-card[data-split-type]'));
}

// Returns all dynamically created system chart cards (batteries/UPS)
function _getDynamicSystemCards() {
    return Array.from(document.querySelectorAll('.chart-card[data-system-chart]'));
}

// Returns all dynamically created app chart cards (nginx, containers, postgres, custom)
function _getAppCards() {
    return Array.from(document.querySelectorAll('.chart-card[data-app-chart]'));
}

export function toggleFocusMode() {
    const grids = document.querySelectorAll('.charts-grid');
    const btn = document.getElementById('btn-focus');

    if (state.focusMode && !state.focusSelecting) {
        // Exit focus mode
        state.focusMode = false;
        grids.forEach(g => g.classList.remove('focus-active', 'focus-selecting', 'focus-hidden'));
        document.querySelectorAll('.section-title').forEach(t => t.classList.remove('focus-active', 'focus-selecting', 'focus-hidden'));
        btn.classList.remove('focus-active');
        document.getElementById('gauges-row')?.classList.remove('focus-hidden');
        document.getElementById('chart-search')?.classList.remove('focus-hidden');
        chartCardIds.forEach(id => {
            const el = document.getElementById(id);
            if (el) el.classList.remove('focus-visible', 'focus-selected');
        });
        _getSplitCards().forEach(card => card.classList.remove('focus-visible', 'focus-selected'));
        _getDynamicSystemCards().forEach(card => card.classList.remove('focus-visible', 'focus-selected'));
        _getAppCards().forEach(card => card.classList.remove('focus-visible', 'focus-selected'));
        removeFocusBar();
        localStorage.removeItem('kula_focus_visible');
        state.focusVisible = null;
        restoreGrids();
        return;
    }

    if (state.focusSelecting) {
        // Apply selection
        const selected = [];
        chartCardIds.forEach(id => {
            const el = document.getElementById(id);
            if (el?.classList.contains('focus-selected')) selected.push(id);
        });
        _getSplitCards().forEach(card => {
            if (card.classList.contains('focus-selected')) selected.push(card.id);
        });
        _getDynamicSystemCards().forEach(card => {
            if (card.classList.contains('focus-selected')) selected.push(card.id);
        });
        _getAppCards().forEach(card => {
            if (card.classList.contains('focus-selected')) selected.push(card.id);
        });

        if (selected.length === 0) {
            // No selection = exit
            state.focusMode = false;
            state.focusSelecting = false;
            grids.forEach(g => g.classList.remove('focus-active', 'focus-selecting'));
            document.querySelectorAll('.section-title').forEach(t => t.classList.remove('focus-active', 'focus-selecting'));
            btn.classList.remove('focus-active');
            removeFocusBar();
            restoreGrids();
            return;
        }

        state.focusVisible = selected;
        localStorage.setItem('kula_focus_visible', JSON.stringify(selected));
        state.focusSelecting = false;

        grids.forEach(g => {
            g.classList.remove('focus-selecting');
            const hasVisible = Array.from(g.querySelectorAll('.chart-card')).some(el => {
                const id = el.id;
                const isSelected = selected.includes(id);
                const isHidden = el.classList.contains('hidden');
                return isSelected && !isHidden;
            });
            g.classList.toggle('focus-active', hasVisible);
            g.classList.toggle('focus-hidden', !hasVisible);
        });

        document.querySelectorAll('.section-title').forEach(t => {
            t.classList.remove('focus-selecting');
            const grid = t.nextElementSibling;
            const hasVisible = grid?.classList.contains('charts-grid') && grid.classList.contains('focus-active');
            t.classList.toggle('focus-active', !!hasVisible);
            t.classList.toggle('focus-hidden', !hasVisible);
        });

        chartCardIds.forEach(id => {
            const el = document.getElementById(id);
            if (el) {
                const isSelected = selected.includes(id);
                const isHidden = el.classList.contains('hidden');
                el.classList.toggle('focus-visible', isSelected && !isHidden);
                el.classList.remove('focus-selected');
            }
        });
        _getSplitCards().forEach(card => {
            const isSelected = selected.includes(card.id);
            const isHidden = card.classList.contains('hidden');
            card.classList.toggle('focus-visible', isSelected && !isHidden);
            card.classList.remove('focus-selected');
        });
        _getDynamicSystemCards().forEach(card => {
            const isSelected = selected.includes(card.id);
            const isHidden = card.classList.contains('hidden');
            card.classList.toggle('focus-visible', isSelected && !isHidden);
            card.classList.remove('focus-selected');
        });
        _getAppCards().forEach(card => {
            const isSelected = selected.includes(card.id);
            card.classList.toggle('focus-visible', isSelected);
            card.classList.remove('focus-selected');
        });

        if (localStorage.getItem('kula_focus_hide_gauges') === 'true') {
            document.getElementById('gauges-row')?.classList.add('focus-hidden');
        }
        document.getElementById('chart-search')?.classList.add('focus-hidden');

        combineGrids();
        removeFocusBar();
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
        t.classList.add('focus-selecting');
        t.classList.remove('focus-active', 'focus-hidden');
    });
    btn.classList.add('focus-active');

    // Pre-select previously visible cards
    chartCardIds.forEach(id => {
        const el = document.getElementById(id);
        if (el) {
            if (state.focusVisible?.includes(id)) {
                el.classList.add('focus-selected');
            } else {
                el.classList.remove('focus-selected');
            }
        }
    });
    _getSplitCards().forEach(card => {
        if (!card.classList.contains('hidden')) {
            if (state.focusVisible?.includes(card.id)) {
                card.classList.add('focus-selected');
            } else {
                card.classList.remove('focus-selected');
            }
        }
    });
    _getDynamicSystemCards().forEach(card => {
        if (!card.classList.contains('hidden')) {
            if (state.focusVisible?.includes(card.id)) {
                card.classList.add('focus-selected');
            } else {
                card.classList.remove('focus-selected');
            }
        }
    });
    _getAppCards().forEach(card => {
        if (state.focusVisible?.includes(card.id)) {
            card.classList.add('focus-selected');
        } else {
            card.classList.remove('focus-selected');
        }
    });

    showFocusBar();

    // Click handler for selection
    chartCardIds.forEach(id => {
        const el = document.getElementById(id);
        if (el && !el.classList.contains('hidden')) {
            el._focusClick = () => el.classList.toggle('focus-selected');
            el.addEventListener('click', el._focusClick);
        }
    });
    _getSplitCards().forEach(card => {
        if (!card.classList.contains('hidden')) {
            card._focusClick = () => card.classList.toggle('focus-selected');
            card.addEventListener('click', card._focusClick);
        }
    });
    _getDynamicSystemCards().forEach(card => {
        if (!card.classList.contains('hidden')) {
            card._focusClick = () => card.classList.toggle('focus-selected');
            card.addEventListener('click', card._focusClick);
        }
    });
    _getAppCards().forEach(card => {
        card._focusClick = () => card.classList.toggle('focus-selected');
        card.addEventListener('click', card._focusClick);
    });
}

export function showFocusBar() {
    removeFocusBar();
    const bar = document.createElement('div');
    bar.className = 'focus-bar';
    bar.id = 'focus-bar';
    const hideGauges = localStorage.getItem('kula_focus_hide_gauges') === 'true';
    
    const spanWrapper = document.createElement('span');
    const spanText = document.createElement('span');
    spanText.setAttribute('data-i18n', 'select_graphs');
    spanText.textContent = 'Select graphs to display, then click Done';
    spanWrapper.appendChild(spanText);

    const label = document.createElement('label');
    label.className = 'focus-bar-checkbox';
    label.style.display = 'flex';
    label.style.alignItems = 'center';
    label.style.gap = '0.4rem';
    label.style.margin = '0 0.5rem';
    label.style.cursor = 'pointer';

    const chk = document.createElement('input');
    chk.type = 'checkbox';
    chk.id = 'focus-hide-gauges-chk';
    chk.checked = hideGauges;

    const chkSpan = document.createElement('span');
    chkSpan.setAttribute('data-i18n', 'hide_gauges');
    chkSpan.textContent = 'Hide gauges';

    label.appendChild(chk);
    label.appendChild(chkSpan);

    const btnDone = document.createElement('button');
    btnDone.id = 'btn-focus-done';
    btnDone.setAttribute('data-i18n', 'done');
    btnDone.textContent = 'Done';

    const btnCancel = document.createElement('button');
    btnCancel.id = 'btn-focus-cancel';
    btnCancel.setAttribute('data-i18n', 'cancel');
    btnCancel.textContent = 'Cancel';

    bar.appendChild(spanWrapper);
    bar.appendChild(label);
    bar.appendChild(btnDone);
    bar.appendChild(btnCancel);

    const firstGrid = document.querySelector('.charts-grid');
    if (firstGrid) firstGrid.parentNode.insertBefore(bar, firstGrid);

    chk.addEventListener('change', (e) => {
        localStorage.setItem('kula_focus_hide_gauges', e.target.checked ? 'true' : 'false');
    });

    btnDone.addEventListener('click', toggleFocusMode);
    btnCancel.addEventListener('click', () => {
        state.focusSelecting = false;
        state.focusMode = false;
        document.querySelectorAll('.charts-grid').forEach(g => g.classList.remove('focus-selecting', 'focus-hidden'));
        document.querySelectorAll('.section-title').forEach(t => t.classList.remove('focus-selecting', 'focus-hidden'));
        document.getElementById('btn-focus').classList.remove('focus-active');
        document.getElementById('gauges-row')?.classList.remove('focus-hidden');
        document.getElementById('chart-search')?.classList.remove('focus-hidden');
        removeFocusBar();
        restoreGrids();
    });

    if (typeof applyTranslation === 'function') {
        applyTranslation(document.getElementById('focus-bar'));
    }
}

export function removeFocusBar() {
    const bar = document.getElementById('focus-bar');
    if (bar) bar.remove();
    chartCardIds.forEach(id => {
        const el = document.getElementById(id);
        if (el) {
            if (el._focusClick) {
                el.removeEventListener('click', el._focusClick);
                delete el._focusClick;
            }
        }
    });
    _getSplitCards().forEach(card => {
        if (card._focusClick) {
            card.removeEventListener('click', card._focusClick);
            delete card._focusClick;
        }
    });
    _getDynamicSystemCards().forEach(card => {
        if (card._focusClick) {
            card.removeEventListener('click', card._focusClick);
            delete card._focusClick;
        }
    });
    _getAppCards().forEach(card => {
        if (card._focusClick) {
            card.removeEventListener('click', card._focusClick);
            delete card._focusClick;
        }
    });
}

export function applyStoredFocusMode() {
    if (state.focusVisible && state.focusVisible.length > 0) {
        state.focusMode = true;
        document.getElementById('btn-focus')?.classList.add('focus-active');

        const grids = document.querySelectorAll('.charts-grid');
        grids.forEach(g => {
            const hasVisible = Array.from(g.querySelectorAll('.chart-card')).some(el => {
                const id = el.id;
                const isSelected = state.focusVisible.includes(id);
                const isHidden = el.classList.contains('hidden');
                return isSelected && !isHidden;
            });
            g.classList.toggle('focus-active', hasVisible);
            g.classList.toggle('focus-hidden', !hasVisible);
        });

        document.querySelectorAll('.section-title').forEach(t => {
            const grid = t.nextElementSibling;
            const hasVisible = grid?.classList.contains('charts-grid') && grid.classList.contains('focus-active');
            t.classList.toggle('focus-active', !!hasVisible);
            t.classList.toggle('focus-hidden', !hasVisible);
        });

        chartCardIds.forEach(id => {
            const el = document.getElementById(id);
            if (el) {
                const isSelected = state.focusVisible.includes(id);
                const isHidden = el.classList.contains('hidden');
                // Only show if selected AND not logically hidden by telemetry
                el.classList.toggle('focus-visible', isSelected && !isHidden);
            }
        });
        _getSplitCards().forEach(card => {
            const isSelected = state.focusVisible.includes(card.id);
            const isHidden = card.classList.contains('hidden');
            card.classList.toggle('focus-visible', isSelected && !isHidden);
        });
        _getDynamicSystemCards().forEach(card => {
            const isSelected = state.focusVisible.includes(card.id);
            const isHidden = card.classList.contains('hidden');
            card.classList.toggle('focus-visible', isSelected && !isHidden);
        });
        _getAppCards().forEach(card => {
            const isSelected = state.focusVisible.includes(card.id);
            card.classList.toggle('focus-visible', isSelected);
        });

        if (localStorage.getItem('kula_focus_hide_gauges') === 'true') {
            document.getElementById('gauges-row')?.classList.add('focus-hidden');
        }
        document.getElementById('chart-search')?.classList.add('focus-hidden');

        combineGrids();
    }
}

export function combineGrids() {
    const mainGrid = document.getElementById('charts-grid');
    if (!mainGrid) return;
    chartCardIds.forEach(id => {
        const el = document.getElementById(id);
        if (el) mainGrid.appendChild(el);
    });
    // Also move split cards into the main grid for a unified focus layout
    _getSplitCards().forEach(card => mainGrid.appendChild(card));
    // Keep dynamic system cards after the canonical system and split cards.
    _getDynamicSystemCards().forEach(card => mainGrid.appendChild(card));
    // Move app cards (nginx, containers, postgres, custom) into the main grid
    _getAppCards().forEach(card => mainGrid.appendChild(card));
}

export function restoreGrids() {
    const mainGrid = document.getElementById('charts-grid');
    const thermalsGrid = document.getElementById('thermals-grid');
    if (!mainGrid || !thermalsGrid) return;

    // Restore static cards to their canonical grids
    const thermalsIds = ['card-cpu-temp', 'card-disk-temp', 'card-gpu-temp'];
    chartCardIds.forEach(id => {
        const el = document.getElementById(id);
        if (el) {
            if (thermalsIds.includes(id)) {
                thermalsGrid.appendChild(el);
            } else {
                mainGrid.appendChild(el);
            }
        }
    });

    // Re-insert split cards after their respective anchor cards so they stay
    // in the correct position (e.g. network splits after card-pps, not at the end).
    const insertConfigs = [
        { afterId: 'card-pps',        type: 'network',   filter: null },
        { afterId: 'card-disk-io',    type: 'diskio',    filter: null },
        { afterId: 'card-disk-space', type: 'diskspace', filter: null },
        { afterId: 'card-vram',       type: 'gpu',       filter: id => !id.startsWith('card-split-gputemp-') },
        { afterId: 'card-disk-temp',  type: 'disktemp',  filter: null },
        { afterId: 'card-gpu-temp',   type: 'gpu',       filter: id => id.startsWith('card-split-gputemp-') },
    ];
    for (const cfg of insertConfigs) {
        const anchor = document.getElementById(cfg.afterId);
        if (!anchor) continue;
        let insertAfter = anchor;
        const cards = Array.from(document.querySelectorAll(`[data-split-type="${cfg.type}"]`))
            .filter(card => !cfg.filter || cfg.filter(card.id));
        for (const card of cards) {
            insertAfter.insertAdjacentElement('afterend', card);
            insertAfter = card;
        }
    }

    // Dynamic system cards are normally created at the end of the system grid.
    _getDynamicSystemCards().forEach(card => mainGrid.appendChild(card));

    // Restore app cards back to applications-grid
    const appsGrid = document.getElementById('applications-grid');
    if (appsGrid) {
        _getAppCards().forEach(card => appsGrid.appendChild(card));
    }
}
