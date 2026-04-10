/* ============================================================
   ui-actions.js — Hover-pause on chart cards, chart
   expand/collapse, and per-chart settings (Y-axis bounds).
   ============================================================ */
'use strict';
import { state } from './state.js';
import { initCharts } from './charts-init.js';
import { fetchHistory } from './charts-data.js';
import { syncPauseState } from './controls.js';

// ---- Hover Pause ----
export function setupHoverPause() {
    document.querySelectorAll('.chart-card').forEach(card => {
        card.addEventListener('mouseenter', () => {
            if (!state.pausedHover) {
                state.pausedHover = true;
                syncPauseState();
            }
        });
        card.addEventListener('mouseleave', () => {
            if (state.pausedHover) {
                state.pausedHover = false;
                syncPauseState();
            }
        });

        // Touch events for mobile
        card.addEventListener('touchstart', () => {
            if (!state.pausedHover) {
                state.pausedHover = true;
                syncPauseState();
            }
        }, { passive: true });

        const resumeFromTouch = () => {
            if (state.pausedHover) {
                state.pausedHover = false;
                syncPauseState();
            }
            const canvas = card.querySelector('canvas');
            if (canvas) {
                const chart = Object.values(state.charts).find(c => c && c.canvas === canvas);
                if (chart && chart.tooltip) {
                    chart.tooltip.setActiveElements([], { x: 0, y: 0 });
                    chart.update();
                }
            }
        };

        card.addEventListener('touchend', resumeFromTouch, { passive: true });
        card.addEventListener('touchcancel', resumeFromTouch, { passive: true });
    });
}

// ---- Chart Expand / Collapse ----
export function toggleExpandChart(cardId) {
    const card = document.getElementById(cardId);
    if (!card) return;

    const grid = card.closest('.charts-grid');
    if (!grid) return;
    // Capture all currently visible cards from this grid
    const visibleCards = Array.from(grid.querySelectorAll('.chart-card:not(.hidden)'));
    const isExpanding = !card.classList.contains('chart-expanded');

    if (isExpanding) {
        // Baseline orders to DOM index if no cards are currently expanded
        const hasAnyExpanded = visibleCards.some(c => c.classList.contains('chart-expanded'));
        if (!hasAnyExpanded) {
            visibleCards.forEach((c, idx) => {
                c.style.order = (idx + 1) * 10;
            });
        }

        // Find all cards physically aligned on the same row vertically
        const myTop = card.offsetTop;
        const sameRowCards = visibleCards.filter(c => Math.abs(c.offsetTop - myTop) < 10);

        if (sameRowCards.length > 0) {
            // Sort by physical horizontal offset to reliably find the first one on this row
            sameRowCards.sort((a, b) => a.offsetLeft - b.offsetLeft);
            const firstInRow = sameRowCards[0];

            // Extract its current order or construct it
            const parsed = parseInt(firstInRow.style.order, 10);
            const firstOrder = Number.isNaN(parsed) ? ((visibleCards.indexOf(firstInRow) + 1) * 10) : parsed;

            // Jump the expanding card to the front of this logical row, pushing others down
            card.style.order = firstOrder - 5;
        }
    }

    const isExpanded = card.classList.toggle('chart-expanded');

    if (!isExpanded) {
        // Restore natural DOM sequence order manually
        const domIndex = visibleCards.indexOf(card);
        card.style.order = (domIndex + 1) * 10;

        // Optional cleanup - if no expanders remain, clear inline orders entirely
        const hasAnyExpanded = visibleCards.some(c => c.classList.contains('chart-expanded'));
        if (!hasAnyExpanded) {
            visibleCards.forEach(c => c.style.order = '');
        }
    }

    const btn = card.querySelector('.btn-expand-chart');
    if (btn) btn.title = isExpanded ? 'Collapse chart' : 'Expand chart';
    if (btn) btn.textContent = isExpanded ? '🔍' : '🔍';

    // Resize the Chart.js instance so it fills the new dimensions
    const canvas = card.querySelector('canvas');
    if (canvas) {
        const chartInst = Object.values(state.charts).find(c => c && c.canvas === canvas);
        if (chartInst) {
            setTimeout(() => chartInst.resize(), 50);
        }
    }
}

// ---- Chart Header Actions (expand button + settings dropdown) ----
const _docClickListeners = [];

export function setupChartActions() {
    // Remove any previously registered document-level dropdown dismiss listeners.
    for (const fn of _docClickListeners) document.removeEventListener('click', fn);
    _docClickListeners.length = 0;

    document.querySelectorAll('.chart-card').forEach(card => {
        const header = card.querySelector('.chart-header');
        if (!header) return;

        header.style.position = 'relative';

        // Reuse existing right group (where selectors live) or create a new one
        let actions = header.querySelector('.chart-header-right');
        if (!actions) {
            actions = document.createElement('div');
            actions.className = 'chart-header-right';
            header.appendChild(actions);
        }

        actions.id = card.id + '-actions';
        actions.style.marginLeft = 'auto';
        actions.style.display = 'flex';
        actions.style.alignItems = 'center';
        actions.style.gap = '0.35rem';

        // Clean up any old injected buttons or dropdowns
        header.querySelectorAll('.btn-icon, .alert-dropdown, .chart-settings-dropdown').forEach(el => el.remove());

        // Check if this graph needs a settings button
        let graphId = null;
        if (card.id === 'card-cpu-temp') graphId = 'cpu_temp';
        else if (card.id === 'card-disk-temp') graphId = 'disk_temp';
        else if (card.id === 'card-gpu-temp') graphId = 'gpu_temp';
        else if (card.id === 'card-network') graphId = 'network';

        if (graphId) {
            const sBtn = document.createElement('button');
            sBtn.className = 'btn-icon';
            sBtn.title = 'Graph Bounds';
            sBtn.textContent = '⚙️';
            sBtn.style.fontSize = '0.85rem';
            sBtn.style.padding = '0.15rem 0.35rem';
            sBtn.style.opacity = '0.5';
            sBtn.style.transition = 'opacity 0.15s';
            sBtn.onmouseenter = () => sBtn.style.opacity = '1';
            sBtn.onmouseleave = () => sBtn.style.opacity = '0.5';

            const dropdown = document.createElement('div');
            dropdown.className = 'chart-settings-dropdown hidden';

            const title = document.createElement('div');
            title.style.marginBottom = '0.5rem';
            title.style.fontSize = '0.75rem';
            title.style.fontWeight = '600';
            title.style.textTransform = 'uppercase';
            title.style.color = 'var(--text-muted)';
            title.textContent = 'Y-Axis Limit';

            const select = document.createElement('select');
            select.style.width = '100%';
            select.style.marginBottom = '0.5rem';
            select.style.padding = '0.3rem';
            select.style.borderRadius = 'var(--radius-sm)';
            select.style.border = '1px solid var(--border)';
            select.style.background = 'var(--bg-card)';
            select.style.color = 'var(--text)';
            select.style.fontSize = '0.85rem';
            select.innerHTML = `
                <option value="off">Off (Auto-scale)</option>
                <option value="on">On (Max Limit)</option>
            `;

            const input = document.createElement('input');
            input.type = 'number';
            input.placeholder = graphId === 'network' ? 'Mbps' : '°C';
            input.style.width = '100%';
            input.style.padding = '0.3rem';
            input.style.borderRadius = 'var(--radius-sm)';
            input.style.border = '1px solid var(--border)';
            input.style.background = 'var(--bg-card)';
            input.style.color = 'var(--text)';
            input.style.fontSize = '0.85rem';

            select.addEventListener('change', () => {
                input.style.display = select.value === 'off' ? 'none' : 'block';
            });

            const saveBtn = document.createElement('button');
            saveBtn.textContent = 'Apply';
            saveBtn.style.width = '100%';
            saveBtn.style.marginTop = '0.75rem';
            saveBtn.style.padding = '0.4rem';
            saveBtn.style.borderRadius = 'var(--radius-sm)';
            saveBtn.style.background = 'var(--accent-blue)';
            saveBtn.style.color = '#fff';
            saveBtn.style.border = 'none';
            saveBtn.style.cursor = 'pointer';
            saveBtn.style.fontSize = '0.85rem';
            saveBtn.style.fontWeight = '500';

            dropdown.appendChild(title);
            dropdown.appendChild(select);
            dropdown.appendChild(input);
            dropdown.appendChild(saveBtn);

            header.appendChild(dropdown);
            actions.appendChild(sBtn);

            sBtn.addEventListener('click', (e) => {
                e.stopPropagation();
                let prefs = {};
                try { prefs = JSON.parse(localStorage.getItem('kula_graphs_max') || '{}'); } catch (e) { }
                let cur = prefs[graphId] || (state.configMax && state.configMax[graphId]);
                if (!cur || !cur.mode) cur = Object.assign({}, cur, { mode: 'off', value: cur?.value || (graphId === 'network' ? 1000 : 100) });

                let uiMode = (cur.mode === 'auto' || cur.mode === 'on') ? 'on' : 'off';
                let uiVal = cur.value;
                if (cur.mode === 'auto') {
                    uiVal = (typeof cur.auto === 'number' && cur.auto > 0) ? cur.auto : cur.value;
                }
                if (uiMode === 'off') {
                    // Keep a sensible default loaded behind the scenes so if they toggle 'on' they see a valid prompt
                    if (!uiVal) uiVal = (graphId === 'network') ? 1000 : 100;
                    if (cur.auto && cur.auto > 0) uiVal = cur.auto;
                }

                select.value = uiMode;
                input.value = uiVal;
                input.style.display = uiMode === 'off' ? 'none' : 'block';

                document.querySelectorAll('.chart-settings-dropdown').forEach(d => {
                    if (d !== dropdown) {
                        d.classList.add('hidden');
                    }
                });

                dropdown.classList.toggle('hidden');
            });

            select.addEventListener('click', e => e.stopPropagation());
            input.addEventListener('click', e => e.stopPropagation());
            title.addEventListener('click', e => e.stopPropagation());
            dropdown.addEventListener('click', e => e.stopPropagation());

            saveBtn.addEventListener('click', (e) => {
                e.stopPropagation();
                let prefs = {};
                try { prefs = JSON.parse(localStorage.getItem('kula_graphs_max') || '{}'); } catch (e) { }
                prefs[graphId] = {
                    mode: select.value,
                    value: parseFloat(input.value) || (graphId === 'network' ? 1000 : 100)
                };
                localStorage.setItem('kula_graphs_max', JSON.stringify(prefs));
                dropdown.classList.add('hidden');

                initCharts();
                fetchHistory(state.timeRange);
            });

            const dismissDropdown = (e) => {
                if (!dropdown.classList.contains('hidden') && !dropdown.contains(e.target) && e.target !== sBtn) {
                    dropdown.classList.add('hidden');
                }
            };
            document.addEventListener('click', dismissDropdown);
            _docClickListeners.push(dismissDropdown);
        }

        const btn = document.createElement('button');
        btn.className = 'btn-icon btn-expand-chart';
        btn.title = 'Expand chart';
        btn.textContent = '🔍';
        btn.addEventListener('click', (e) => {
            e.stopPropagation();
            toggleExpandChart(card.id);
        });

        actions.appendChild(btn);
    });
}
