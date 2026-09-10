/* ============================================================
   main.js — Application entry point. Wires up all event
   listeners and kicks off auth + WebSocket connection.
   Must be loaded LAST after all other modules.
   ============================================================ */
'use strict';
import { state } from './state.js';
import { initCharts } from './charts-init.js';
import { i18n } from './i18n.js';
import {
    fetchCustomHistory,
    fetchHistory,
    redrawChartsFromBuffer,
    renderSamplingInfo,
    syncZoom,
    updateAllCharts,
} from './charts-data.js';
import { toggleAlertDropdown } from './alerts.js';
import { initSystemInfo } from './system-info.js';
import {
    syncPauseState,
    togglePause,
    toggleLayout,
    applyLayout,
    setTimeRange,
    toggleCustomTimePicker,
    initCustomTimePicker,
    refreshCustomTimePicker,
    initializeViewportNavigation,
    commitGestureViewport,
    historyBack,
    historyForward,
    goLive,
    zoomOutViewport,
    reloadCurrentHistory,
    syncTimeRangeUI,
    syncCustomRangeUI,
} from './controls.js';
import { applyUrlState, updateUrl } from './history-navigation.js';
import { checkAuth, handleLogin, handleLogout } from './auth.js';
import { aggregationField } from './history-data.js';
import { addExpandButton, attachHoverPauseToCard } from './chart-card-actions.js';
import { toggleFocusMode, applyStoredFocusMode } from './focus-mode.js';
import { initSplitModule } from './split.js';
import { initOllama } from './ollama.js';
import { chartsGridForTitle, sectionHeadForTitle } from './section-utils.js';
import {
    applyServerSettings,
    applySettings,
    applyTheme,
    getSetting,
    initSettingsMenu,
    toggleTheme,
} from './settings.js';
import { updateGauges } from './gauges.js';
import { forEachRegisteredChart, setSharedCrosshair } from './chart-controller.js';
import { sharedCrosshair } from './chart-interactions.js';
import { announceChartCursor, updateChartAccessibility } from './chart-accessibility.js';
import { formatFullTimestamp, normalizeTimeZone } from './format.js';
import { updateHeader } from './header.js';

function setupHoverPause() {
    document.querySelectorAll('.chart-card').forEach(attachHoverPauseToCard);
}

// Document-level dropdown listeners are replaced whenever charts are rebuilt.
const chartActionDismissListeners = [];

function setupChartActions() {
    for (const listener of chartActionDismissListeners) {
        document.removeEventListener('click', listener);
    }
    chartActionDismissListeners.length = 0;

    document.querySelectorAll('.chart-card').forEach(card => {
        const header = card.querySelector('.chart-header');
        if (!header) return;

        header.style.position = 'relative';

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

        header.querySelectorAll('.btn-icon, .alert-dropdown, .chart-settings-dropdown').forEach(el => el.remove());

        let graphId = null;
        if (card.id === 'card-cpu-temp') graphId = 'cpu_temp';
        else if (card.id === 'card-disk-temp') graphId = 'disk_temp';
        else if (card.id === 'card-gpu-temp') graphId = 'gpu_temp';
        else if (card.id === 'card-network') graphId = 'network';

        if (graphId) {
            const settingsButton = document.createElement('button');
            settingsButton.className = 'btn-icon';
            settingsButton.title = 'Graph Bounds';
            settingsButton.textContent = '⚙️';
            settingsButton.style.fontSize = '0.85rem';
            settingsButton.style.padding = '0.15rem 0.35rem';
            settingsButton.style.opacity = '0.5';
            settingsButton.style.transition = 'opacity 0.15s';
            settingsButton.onmouseenter = () => settingsButton.style.opacity = '1';
            settingsButton.onmouseleave = () => settingsButton.style.opacity = '0.5';

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

            const saveButton = document.createElement('button');
            saveButton.textContent = 'Apply';
            saveButton.style.width = '100%';
            saveButton.style.marginTop = '0.75rem';
            saveButton.style.padding = '0.4rem';
            saveButton.style.borderRadius = 'var(--radius-sm)';
            saveButton.style.background = 'var(--accent-blue)';
            saveButton.style.color = '#fff';
            saveButton.style.border = 'none';
            saveButton.style.cursor = 'pointer';
            saveButton.style.fontSize = '0.85rem';
            saveButton.style.fontWeight = '500';

            dropdown.appendChild(title);
            dropdown.appendChild(select);
            dropdown.appendChild(input);
            dropdown.appendChild(saveButton);

            header.appendChild(dropdown);
            actions.appendChild(settingsButton);

            settingsButton.addEventListener('click', event => {
                event.stopPropagation();
                let preferences = {};
                try {
                    preferences = JSON.parse(localStorage.getItem('kula_graphs_max') || '{}');
                } catch (error) { /* Invalid stored preferences fall back to defaults. */ }
                let current = preferences[graphId] || (state.configMax && state.configMax[graphId]);
                if (!current || !current.mode) {
                    current = {
                        mode: 'off',
                        value: current?.value || (graphId === 'network' ? 1000 : 100),
                        auto: current?.auto,
                    };
                }

                const uiMode = (current.mode === 'auto' || current.mode === 'on') ? 'on' : 'off';
                let uiValue = current.value;
                if (current.mode === 'auto') {
                    uiValue = (typeof current.auto === 'number' && current.auto > 0)
                        ? current.auto
                        : current.value;
                }
                if (uiMode === 'off') {
                    if (!uiValue) uiValue = graphId === 'network' ? 1000 : 100;
                    if (current.auto && current.auto > 0) uiValue = current.auto;
                }

                select.value = uiMode;
                input.value = uiValue;
                input.style.display = uiMode === 'off' ? 'none' : 'block';

                document.querySelectorAll('.chart-settings-dropdown').forEach(other => {
                    if (other !== dropdown) other.classList.add('hidden');
                });
                dropdown.classList.toggle('hidden');
            });

            select.addEventListener('click', event => event.stopPropagation());
            input.addEventListener('click', event => event.stopPropagation());
            title.addEventListener('click', event => event.stopPropagation());
            dropdown.addEventListener('click', event => event.stopPropagation());

            saveButton.addEventListener('click', event => {
                event.stopPropagation();
                let preferences = {};
                try {
                    preferences = JSON.parse(localStorage.getItem('kula_graphs_max') || '{}');
                } catch (error) { /* Invalid stored preferences are replaced. */ }
                preferences[graphId] = {
                    mode: select.value,
                    value: parseFloat(input.value) || (graphId === 'network' ? 1000 : 100),
                };
                localStorage.setItem('kula_graphs_max', JSON.stringify(preferences));
                dropdown.classList.add('hidden');

                initCharts();
                if (state.timeRange !== null) {
                    fetchHistory(state.timeRange);
                } else if (state.customFrom && state.customTo) {
                    fetchCustomHistory(state.customFrom, state.customTo);
                }
            });

            const dismissDropdown = event => {
                if (!dropdown.classList.contains('hidden') &&
                    !dropdown.contains(event.target) && event.target !== settingsButton) {
                    dropdown.classList.add('hidden');
                }
            };
            document.addEventListener('click', dismissDropdown);
            chartActionDismissListeners.push(dismissDropdown);
        }

        addExpandButton(card);
    });
}

function crosshairSnapshot() {
    return {
        timestamp: sharedCrosshair.pinned ?? sharedCrosshair.hovered,
        pinned: sharedCrosshair.pinned !== null,
    };
}

function renderCrosshairTime(crosshair, announce = false) {
    const label = document.getElementById('pinned-time');
    const showPinnedTime = crosshair.pinned && Number.isFinite(crosshair.timestamp);
    const timestampText = showPinnedTime
        ? formatFullTimestamp(crosshair.timestamp, state.timeZone, i18n.currentLang)
        : '';
    if (label) {
        label.textContent = timestampText;
        label.classList.toggle('hidden', !showPinnedTime);
        label.classList.toggle('pinned', showPinnedTime);
    }
    if (announce) {
        const announcement = document.getElementById('pinned-time-announcement');
        if (announcement) {
            announcement.textContent = crosshair.pinned
                ? `${i18n.t('pinned_time')}: ${timestampText}`
                : i18n.t('pinned_time_cleared');
        }
    }
}

function syncTimeZoneControls() {
    document.querySelectorAll('.time-zone-btn').forEach(button => {
        const active = button.dataset.timeZone === state.timeZone;
        button.classList.toggle('active', active);
        button.setAttribute('aria-pressed', String(active));
    });
}

function setDisplayTimeZone(mode) {
    const previousZone = state.timeZone;
    state.timeZone = normalizeTimeZone(mode);
    localStorage.setItem('kula_time_zone', state.timeZone);
    syncTimeZoneControls();
    if (state.customFrom && state.customTo) {
        syncCustomRangeUI(state.customFrom, state.customTo, { preserveDraft: true });
    }
    refreshCustomTimePicker(previousZone);
    renderCrosshairTime(crosshairSnapshot());
    if (state.lastSample) updateHeader(state.lastSample);
    updateAllCharts();
}

function filterCharts(query) {
    // Collect all section-title + charts-grid pairs (titles may sit in .section-head)
    const sections = document.querySelectorAll('.section-title');
    sections.forEach(title => {
        const grid = chartsGridForTitle(title);
        if (!grid) return;
        const head = sectionHeadForTitle(title);

        const cards = grid.querySelectorAll('.chart-card');
        let anyVisible = false;
        cards.forEach(card => {
            const h3 = card.querySelector('h3');
            const name = (h3?.textContent || '').toLowerCase();
            // Also match subtitle text for richer search
            const subtitle = card.querySelector('.chart-subtitle');
            const subText = (subtitle?.textContent || '').toLowerCase();
            // Multi-series cards (container metrics) name their series in
            // data-search-terms, since the title only carries the metric name.
            const terms = (card.dataset.searchTerms || '').toLowerCase();
            const match = !query || name.includes(query) || subText.includes(query) || terms.includes(query);
            if (match) {
                card.classList.remove('chart-search-hidden');
                // Don't show cards that are legitimately hidden (e.g., GPU on non-GPU systems)
                if (!card.classList.contains('hidden')) anyVisible = true;
            } else {
                card.classList.add('chart-search-hidden');
            }
        });

        // Hide the section chrome + grid if nothing matches
        if (query) {
            title.classList.toggle('chart-search-hidden', !anyVisible);
            if (head && head !== title) head.classList.toggle('chart-search-hidden', !anyVisible);
            grid.classList.toggle('chart-search-hidden', !anyVisible);
        } else {
            title.classList.remove('chart-search-hidden');
            if (head && head !== title) head.classList.remove('chart-search-hidden');
            grid.classList.remove('chart-search-hidden');
        }
    });
}

async function init() {
    // Initialize i18n before everything else
    await i18n.init();

    // Restore time window / range / aggregation from the URL query string
    // (after i18n so labels render correctly, before the first history fetch).
    const restoredView = applyUrlState(state);
    if (restoredView?.kind === 'custom') {
        syncCustomRangeUI(restoredView.from, restoredView.to);
    } else if (restoredView?.kind === 'preset') {
        syncTimeRangeUI(restoredView.range);
    }
    initializeViewportNavigation();
    syncTimeZoneControls();

    // Apply stored layout
    applyLayout();
    initCharts();

    // Apply the customization settings before the theme: high contrast swaps
    // the CSS custom properties applyTheme() copies into the Chart.js defaults.
    applySettings();

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
    initSystemInfo();
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
    initSettingsMenu();
    document.getElementById('login-form')?.addEventListener('submit', handleLogin);
    document.getElementById('btn-logout')?.addEventListener('click', handleLogout);
    document.getElementById('btn-custom-range').addEventListener('click', toggleCustomTimePicker);
    initCustomTimePicker();
    document.getElementById('btn-live')?.addEventListener('click', goLive);
    document.getElementById('btn-history-back')?.addEventListener('click', historyBack);
    document.getElementById('btn-history-forward')?.addEventListener('click', historyForward);
    document.getElementById('btn-zoom-out')?.addEventListener('click', zoomOutViewport);
    document.querySelectorAll('.time-zone-btn').forEach(button => {
        button.addEventListener('click', () => setDisplayTimeZone(button.dataset.timeZone));
    });

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
            const field = aggregationField(btn.dataset.agg);
            if (!state.validAggregations.includes(field)) return;

            document.querySelectorAll('#agg-presets-list .time-btn').forEach(b => b.classList.remove('active'));
            btn.classList.add('active');
            state.currentAggregation = btn.dataset.agg;
            localStorage.setItem('kula_aggregation', state.currentAggregation);
            updateUrl(state);

            // Redraw charts with new aggregation
            redrawChartsFromBuffer();

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

    // Event delegation also covers split and application charts created after
    // initial page setup.
    document.addEventListener('dblclick', event => {
        if (event.target?.matches?.('.chart-body canvas')) goLive();
    });

    // Hover-pause on chart cards
    setupHoverPause();

    // Expand/Settings actions on chart cards
    setupChartActions();

    // Split buttons (must be after setupChartActions to avoid being removed)
    initSplitModule(redrawChartsFromBuffer);

    // Initialize Ollama AI panel (no-op when disabled)
    // The panel is activated after /api/config is fetched (see auth.js fetchConfig).
    // We hook into the kula-config-ready event dispatched by auth.js.
    document.addEventListener('kula-config-ready', (e) => {
        initOllama(e.detail || {});
        // Server-side appearance/accessibility defaults; stored overrides win.
        applyServerSettings(e.detail || {});
    });

    // Close dropdowns when clicking outside
    document.addEventListener('click', (e) => {
        if (state.alertDropdownOpen && !e.target.closest('#alert-container')) {
            state.alertDropdownOpen = false;
            document.getElementById('alert-dropdown').classList.add('hidden');
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

    // Re-enabling the gauge row leaves it holding whatever was last painted
    // before it was hidden; repaint it from the latest sample right away
    // instead of waiting for the next one to arrive.
    document.addEventListener('kula-settings-changed', () => {
        forEachRegisteredChart(chart => {
            chart.options.plugins.kulaEnvelope.allSeries = getSetting('all_series_envelopes');
            updateChartAccessibility(chart);
        });
        updateAllCharts();
        if (state.lastSample) updateGauges(state.lastSample);
    });

    document.addEventListener('kula-sync-pause', syncPauseState);
    document.addEventListener('kula-i18n-changed', () => {
        syncTimeZoneControls();
        if (state.timeRange !== null) syncTimeRangeUI(state.timeRange);
        else if (state.customFrom && state.customTo) syncCustomRangeUI(state.customFrom, state.customTo, { preserveDraft: true });
        refreshCustomTimePicker();
        renderSamplingInfo();
        renderCrosshairTime(crosshairSnapshot());
        if (state.lastSample) updateHeader(state.lastSample);
        updateAllCharts();
    });
    document.addEventListener('kula-zoom-sync', (e) => syncZoom(e.detail));
    document.addEventListener('kula-viewport-commit', commitGestureViewport);
    document.addEventListener('kula-viewport-reset', goLive);
    document.addEventListener('kula-history-sections-changed', reloadCurrentHistory);
    document.addEventListener('kula-history-metadata-changed', () => updateUrl(state));
    document.addEventListener('kula-crosshair', event => {
        const action = event.detail?.action;
        const crosshair = sharedCrosshair.apply(action, event.detail?.timestamp);
        setSharedCrosshair(crosshair.timestamp);
        // Hover changes are visual only. Announcing every mousemove would
        // overwhelm assistive technology, so the live region changes only on
        // an explicit pin/unpin action.
        const keyboard = event.detail?.keyboard === true;
        renderCrosshairTime(crosshair, !keyboard && ['pin', 'unpin'].includes(action));
        if (keyboard) {
            announceChartCursor(event.detail.chart, crosshair.pinned ? crosshair.timestamp : null);
        }
    });

    checkAuth();
}

if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
} else {
    init();
}
