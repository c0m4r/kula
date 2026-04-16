/* ============================================================
   main.js — Application entry point. Wires up all event
   listeners and kicks off auth + WebSocket connection.
   Must be loaded LAST after all other modules.
   ============================================================ */
'use strict';
import { state } from './state.js';
import { i18n } from './i18n.js';
import { addSampleToCharts, updateAllCharts, clearAllChartData, syncZoom, resetZoomAll, redrawChartsFromBuffer } from './charts-data.js';
import { toggleAlertDropdown, toggleInfoDropdown } from './alerts.js';
import { syncPauseState, togglePause, toggleLayout, applyLayout, setTimeRange, toggleCustomTimePicker, applyCustomRange } from './controls.js';
import { checkAuth, handleLogin, handleLogout } from './auth.js';
import { applyTheme, toggleTheme } from './theme.js';
import { setupHoverPause, setupChartActions } from './ui-actions.js';
import { toggleFocusMode, applyStoredFocusMode } from './focus-mode.js';
import { initSplitModule } from './split.js';
import { initOllama } from './ollama.js';

function filterCharts(query) {
    // Collect all section-title + charts-grid pairs
    const sections = document.querySelectorAll('.section-title');
    sections.forEach(title => {
        const grid = title.nextElementSibling;
        if (!grid || !grid.classList.contains('charts-grid')) return;

        const cards = grid.querySelectorAll('.chart-card');
        let anyVisible = false;
        cards.forEach(card => {
            const h3 = card.querySelector('h3');
            const name = (h3?.textContent || '').toLowerCase();
            // Also match subtitle text for richer search
            const subtitle = card.querySelector('.chart-subtitle');
            const subText = (subtitle?.textContent || '').toLowerCase();
            const match = !query || name.includes(query) || subText.includes(query);
            if (match) {
                card.classList.remove('chart-search-hidden');
                // Don't show cards that are legitimately hidden (e.g., GPU on non-GPU systems)
                if (!card.classList.contains('hidden')) anyVisible = true;
            } else {
                card.classList.add('chart-search-hidden');
            }
        });

        // Hide the section title + grid if nothing matches
        if (query) {
            title.classList.toggle('chart-search-hidden', !anyVisible);
            grid.classList.toggle('chart-search-hidden', !anyVisible);
        } else {
            title.classList.remove('chart-search-hidden');
            grid.classList.remove('chart-search-hidden');
        }
    });
}

async function init() {
    // Initialize i18n before everything else
    await i18n.init();

    // Apply stored layout
    applyLayout();

    // Apply stored theme
    applyTheme();

    // Re-apply theme when OS preference changes (only effective in auto mode)
    window.matchMedia('(prefers-color-scheme: light)').addEventListener('change', () => {
        if (state.theme === 'auto') applyTheme();
    });

    // Apply stored focus mode
    applyStoredFocusMode();

    // Event listeners
    document.getElementById('btn-theme').addEventListener('click', toggleTheme);
    document.getElementById('btn-pause').addEventListener('click', togglePause);
    document.getElementById('btn-layout').addEventListener('click', toggleLayout);
    document.getElementById('btn-alerts').addEventListener('click', toggleAlertDropdown);
    document.getElementById('btn-info').addEventListener('click', toggleInfoDropdown);
    document.getElementById('btn-time-menu').addEventListener('click', (e) => {
        e.stopPropagation();
        const list = document.getElementById('time-presets-list');
        list.classList.toggle('open');
        state.timeDropdownOpen = list.classList.contains('open');
    });
    document.getElementById('btn-agg-menu').addEventListener('click', (e) => {
        e.stopPropagation();
        const list = document.getElementById('agg-presets-list');
        list.classList.toggle('open');
        state.aggDropdownOpen = list.classList.contains('open');
    });
    document.getElementById('btn-focus').addEventListener('click', toggleFocusMode);
    document.getElementById('login-form')?.addEventListener('submit', handleLogin);
    document.getElementById('btn-logout')?.addEventListener('click', handleLogout);
    document.getElementById('btn-custom-range').addEventListener('click', toggleCustomTimePicker);
    document.getElementById('btn-apply-custom').addEventListener('click', applyCustomRange);

    document.querySelectorAll('.time-btn[data-range]').forEach(btn => {
        btn.addEventListener('click', () => {
            setTimeRange(parseInt(btn.dataset.range));
            if (state.timeDropdownOpen) {
                state.timeDropdownOpen = false;
                document.getElementById('time-presets-list').classList.remove('open');
            }
        });
    });

    // Aggregation logic
    document.querySelectorAll('#agg-presets-list .time-btn').forEach(btn => {
        btn.addEventListener('click', () => {
            document.querySelectorAll('#agg-presets-list .time-btn').forEach(b => b.classList.remove('active'));
            btn.classList.add('active');
            state.currentAggregation = btn.dataset.agg;
            localStorage.setItem('kula_aggregation', state.currentAggregation);

            // Redraw charts with new aggregation
            clearAllChartData();
            state.dataBuffer.forEach(item => {
                const timestampSrc = item.data || item;
                const ts = new Date(timestampSrc.ts || item.ts);
                addSampleToCharts(item, ts);
            });
            updateAllCharts();

            if (state.aggDropdownOpen) {
                state.aggDropdownOpen = false;
                document.getElementById('agg-presets-list').classList.remove('open');
            }
        });
    });

    // Initialize active aggregation button
    const aggBtns = document.querySelectorAll('#agg-presets-list .time-btn');
    aggBtns.forEach(b => b.classList.remove('active'));
    const activeAggBtn = document.querySelector(`#agg-presets-list .time-btn[data-agg="${state.currentAggregation}"]`);
    if (activeAggBtn) activeAggBtn.classList.add('active');

    // Double-click on any chart to reset zoom
    document.querySelectorAll('.chart-body canvas').forEach(canvas => {
        canvas.addEventListener('dblclick', resetZoomAll);
    });

    // Hover-pause on chart cards
    setupHoverPause();

    // Expand/Settings actions on chart cards
    setupChartActions();

    // Split buttons (must be after setupChartActions to avoid being removed)
    initSplitModule(redrawChartsFromBuffer, resetZoomAll);

    // Initialize Ollama AI panel (no-op when disabled)
    // The panel is activated after /api/config is fetched (see auth.js fetchConfig).
    // We hook into the kula-config-ready event dispatched by auth.js.
    document.addEventListener('kula-config-ready', (e) => {
        initOllama(e.detail || {});
    });

    // Close dropdowns when clicking outside
    document.addEventListener('click', (e) => {
        if (state.alertDropdownOpen && !e.target.closest('#alert-container')) {
            state.alertDropdownOpen = false;
            document.getElementById('alert-dropdown').classList.add('hidden');
        }
        if (state.infoDropdownOpen && !e.target.closest('#info-container')) {
            state.infoDropdownOpen = false;
            document.getElementById('info-dropdown').classList.add('hidden');
        }
        if (state.timeDropdownOpen && !e.target.closest('.time-presets')) {
            state.timeDropdownOpen = false;
            document.getElementById('time-presets-list').classList.remove('open');
        }
        if (state.aggDropdownOpen && !e.target.closest('#btn-agg-menu') && !e.target.closest('#agg-presets-list')) {
            state.aggDropdownOpen = false;
            document.getElementById('agg-presets-list').classList.remove('open');
        }
        if (!e.target.closest('.btn-icon') && !e.target.closest('.chart-settings-dropdown')) {
            document.querySelectorAll('.chart-settings-dropdown').forEach(d => d.classList.add('hidden'));
        }
    });

    // Chart search/filter
    const searchInput = document.getElementById('chart-search');
    if (searchInput) {
        searchInput.addEventListener('input', () => {
            const query = searchInput.value.trim().toLowerCase();
            filterCharts(query);
        });
    }

    document.addEventListener('kula-sync-pause', syncPauseState);
    document.addEventListener('kula-i18n-changed', updateAllCharts);
    document.addEventListener('kula-zoom-sync', (e) => syncZoom(e.detail));

    checkAuth();
}

if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
} else {
    init();
}
