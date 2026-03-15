/* ============================================================
   focus-mode.js — Focus mode: select and persist a subset of
   chart cards to display.
   ============================================================ */
'use strict';

const chartCardIds = [
    'card-cpu', 'card-loadavg', 'card-memory', 'card-swap',
    'card-network', 'card-pps', 'card-connections',
    'card-disk-io', 'card-disk-space',
    'card-gpu-load', 'card-vram',
    'card-processes', 'card-entropy', 'card-self',
    'card-cpu-temp', 'card-disk-temp', 'card-gpu-temp'
];

function toggleFocusMode() {
    const grids = document.querySelectorAll('.charts-grid');
    const btn = document.getElementById('btn-focus');

    if (state.focusMode && !state.focusSelecting) {
        // Exit focus mode
        state.focusMode = false;
        grids.forEach(g => g.classList.remove('focus-active', 'focus-selecting'));
        document.querySelectorAll('.section-title').forEach(t => t.classList.remove('focus-active', 'focus-selecting'));
        btn.classList.remove('focus-active');
        chartCardIds.forEach(id => {
            const el = document.getElementById(id);
            if (el) el.classList.remove('focus-visible', 'focus-selected');
        });
        removeFocusBar();
        localStorage.removeItem('kula_focus_visible');
        state.focusVisible = null;
        return;
    }

    if (state.focusSelecting) {
        // Apply selection
        const selected = [];
        chartCardIds.forEach(id => {
            const el = document.getElementById(id);
            if (el?.classList.contains('focus-selected')) selected.push(id);
        });

        if (selected.length === 0) {
            // No selection = exit
            state.focusMode = false;
            state.focusSelecting = false;
            grids.forEach(g => g.classList.remove('focus-active', 'focus-selecting'));
            document.querySelectorAll('.section-title').forEach(t => t.classList.remove('focus-active', 'focus-selecting'));
            btn.classList.remove('focus-active');
            removeFocusBar();
            return;
        }

        state.focusVisible = selected;
        localStorage.setItem('kula_focus_visible', JSON.stringify(selected));
        state.focusSelecting = false;
        grids.forEach(g => {
            g.classList.remove('focus-selecting');
            g.classList.add('focus-active');
        });
        document.querySelectorAll('.section-title').forEach(t => {
            t.classList.remove('focus-selecting');
            t.classList.add('focus-active');
        });
        chartCardIds.forEach(id => {
            const el = document.getElementById(id);
            if (el) {
                el.classList.toggle('focus-visible', selected.includes(id));
                el.classList.remove('focus-selected');
            }
        });
        removeFocusBar();
        return;
    }

    // Enter selection mode
    state.focusMode = true;
    state.focusSelecting = true;
    grids.forEach(g => {
        g.classList.add('focus-selecting');
        g.classList.remove('focus-active');
    });
    document.querySelectorAll('.section-title').forEach(t => {
        t.classList.add('focus-selecting');
        t.classList.remove('focus-active');
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

    showFocusBar();

    // Click handler for selection
    chartCardIds.forEach(id => {
        const el = document.getElementById(id);
        if (el) {
            el._focusClick = () => el.classList.toggle('focus-selected');
            el.addEventListener('click', el._focusClick);
        }
    });
}

function showFocusBar() {
    removeFocusBar();
    const bar = document.createElement('div');
    bar.className = 'focus-bar';
    bar.id = 'focus-bar';
    bar.innerHTML = '<span>Select graphs to display, then click Done</span><button id="btn-focus-done">Done</button><button id="btn-focus-cancel">Cancel</button>';
    const firstGrid = document.querySelector('.charts-grid');
    if (firstGrid) firstGrid.parentNode.insertBefore(bar, firstGrid);
    document.getElementById('btn-focus-done').addEventListener('click', toggleFocusMode);
    document.getElementById('btn-focus-cancel').addEventListener('click', () => {
        state.focusSelecting = false;
        state.focusMode = false;
        document.querySelectorAll('.charts-grid').forEach(g => g.classList.remove('focus-selecting'));
        document.querySelectorAll('.section-title').forEach(t => t.classList.remove('focus-selecting'));
        document.getElementById('btn-focus').classList.remove('focus-active');
        removeFocusBar();
    });
}

function removeFocusBar() {
    const bar = document.getElementById('focus-bar');
    if (bar) bar.remove();
    chartCardIds.forEach(id => {
        const el = document.getElementById(id);
        if (el?._focusClick) { el.removeEventListener('click', el._focusClick); delete el._focusClick; }
    });
}

function applyStoredFocusMode() {
    if (state.focusVisible && state.focusVisible.length > 0) {
        state.focusMode = true;
        document.querySelectorAll('.charts-grid').forEach(g => g.classList.add('focus-active'));
        document.querySelectorAll('.section-title').forEach(t => t.classList.add('focus-active'));
        document.getElementById('btn-focus').classList.add('focus-active');
        chartCardIds.forEach(id => {
            const el = document.getElementById(id);
            if (el) el.classList.toggle('focus-visible', state.focusVisible.includes(id));
        });
    }
}
