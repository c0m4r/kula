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

async function init() {
    // Initialize i18n before everything else
    await i18n.init();

    // Apply stored layout
    applyLayout();

    // Apply stored theme
    applyTheme();

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
    initSplitModule(redrawChartsFromBuffer);

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
