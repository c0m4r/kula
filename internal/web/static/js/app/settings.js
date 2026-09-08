/* ============================================================
   settings.js — Theme, appearance, and accessibility
   preferences, persisted per browser.

   Appearance and accessibility settings have a server-side default (web.appearance.* /
   web.accessibility.* in the config, delivered by /api/config).
   The browser only stores the settings the visitor actually
   changed; chart controls are browser preferences. An operator editing the config moves anyone
   who never opened the menu.
   ============================================================ */
'use strict';
import { state } from './state.js';
import { forEachRegisteredChart, queueChartUpdate } from './chart-controller.js';

function resolveTheme() {
    if (state.theme === 'auto') {
        return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
    }
    return state.theme;
}

// Chart.js paints on a canvas, so it can't use CSS custom properties directly.
// Reading them here keeps the canvas synchronized with stylesheet overrides.
function cssVar(name, fallback) {
    const value = getComputedStyle(document.body).getPropertyValue(name).trim();
    return value || fallback;
}

export function applyTheme() {
    const isLight = resolveTheme() === 'light';
    document.body.classList.toggle('light-mode', isLight);

    const gridColor = cssVar('--chart-grid', isLight ? 'rgba(203, 213, 225, 0.4)' : 'rgba(55, 65, 81, 0.2)');
    const textColor = cssVar('--chart-text', isLight ? '#64748b' : '#94a3b8');
    const tooltipBg = cssVar('--chart-tooltip-bg', isLight ? 'rgba(255, 255, 255, 0.95)' : 'rgba(17, 24, 39, 0.9)');
    const tooltipText = cssVar('--chart-tooltip-text', isLight ? '#1e293b' : '#f1f5f9');
    const tooltipBorder = cssVar('--chart-tooltip-border', isLight ? 'rgba(203, 213, 225, 0.8)' : 'rgba(55, 65, 81, 0.5)');

    Chart.defaults.color = textColor;
    Chart.defaults.borderColor = gridColor;
    Chart.defaults.plugins.tooltip.backgroundColor = tooltipBg;
    Chart.defaults.plugins.tooltip.titleColor = tooltipText;
    Chart.defaults.plugins.tooltip.bodyColor = tooltipText;
    Chart.defaults.plugins.tooltip.footerColor = tooltipText;
    Chart.defaults.plugins.tooltip.borderColor = tooltipBorder;
    Chart.defaults.plugins.tooltip.borderWidth = 1;

    forEachRegisteredChart(chart => {
        if (!chart) return;
        if (chart.options.scales.x) {
            chart.options.scales.x.grid.color = gridColor;
            chart.options.scales.x.ticks.color = textColor;
        }
        if (chart.options.scales.y) {
            chart.options.scales.y.grid.color = gridColor;
            chart.options.scales.y.ticks.color = textColor;
        }
        if (chart.options.scales.y1) {
            chart.options.scales.y1.grid.color = gridColor;
            chart.options.scales.y1.ticks.color = textColor;
        }
        if (chart.options.plugins.tooltip) {
            chart.options.plugins.tooltip.backgroundColor = tooltipBg;
            chart.options.plugins.tooltip.titleColor = tooltipText;
            chart.options.plugins.tooltip.bodyColor = tooltipText;
            chart.options.plugins.tooltip.footerColor = tooltipText;
            chart.options.plugins.tooltip.borderColor = tooltipBorder;
            chart.options.plugins.tooltip.borderWidth = 1;
        }
        queueChartUpdate(chart);
    });
}

export function toggleTheme() {
    const effective = resolveTheme();
    state.theme = effective === 'dark' ? 'light' : 'dark';
    localStorage.setItem('kula_theme', state.theme);
    applyTheme();
}

const STORAGE_KEY = 'kula_ui_settings';

// Matches `html { font-size }` in style.css; text_size scales this.
const BASE_FONT_PX = 14;
// Chart.js draws to a canvas in px, so its labels need the same scaling
// applied by hand or they stay put while the rest of the layout grows.
const BASE_CHART_FONT_PX = 11;

// Server-supplied bounds for the text size stepper; /api/config overrides
// these, and they must stay in step with config.MinTextSize/MaxTextSize.
const textSizeRange = { min: 50, max: 300, step: 10 };

const clamp = (n, lo, hi) => Math.min(hi, Math.max(lo, n));

// Each boolean setting maps to the document class that implements it.
// `whenFalse` marks settings whose class is applied when the value is off
// (the on state is the plain stylesheet, so nothing is added in the common
// case). Classes live on <html> so the palette overrides outrank
// `body.light-mode` in both themes.
export const SETTINGS = [
    { key: 'sticky_topbar', group: 'appearance', fallback: true, cls: 'no-sticky-topbar', whenFalse: true, input: 'set-sticky-topbar' },
    { key: 'chart_data_controls', group: 'charts', fallback: false, cls: 'chart-data-controls', input: 'set-chart-data-controls' },
    { key: 'chart_tooltip_details', group: 'charts', fallback: false, cls: 'chart-tooltip-details', input: 'set-chart-tooltip-details' },
    { key: 'all_series_envelopes', group: 'charts', fallback: false, cls: 'all-series-envelopes', input: 'set-all-series-envelopes' },
    { key: 'gauges', group: 'appearance', fallback: true, cls: 'no-gauges', whenFalse: true, input: 'set-gauges' },
    { key: 'high_contrast', group: 'accessibility', fallback: false, cls: 'a11y-contrast', input: 'set-high-contrast' },
    { key: 'reduce_motion', group: 'accessibility', fallback: false, cls: 'a11y-reduce-motion', input: 'set-reduce-motion' },
    { key: 'underline_links', group: 'accessibility', fallback: false, cls: 'a11y-underline-links', input: 'set-underline-links' },
    { key: 'focus_outline', group: 'accessibility', fallback: false, cls: 'a11y-focus-outline', input: 'set-focus-outline' },
    {
        key: 'text_size', group: 'accessibility', type: 'number', fallback: 100,
        apply(pct) {
            document.documentElement.style.fontSize =
                pct === 100 ? '' : (BASE_FONT_PX * pct / 100).toFixed(3) + 'px';
            Chart.defaults.font.size = BASE_CHART_FONT_PX * pct / 100;
        },
    },
];

const byKey = new Map(SETTINGS.map(s => [s.key, s]));

// Defaults from /api/config; empty until the config request lands.
let serverDefaults = {};
// Only the settings the visitor explicitly changed.
let overrides = readOverrides();

// Reject anything that is not a valid value for a known setting, so a stale
// or hand-edited entry can't smuggle in a class name or a broken font size.
function sanitize(key, value) {
    const def = byKey.get(key);
    if (!def) return undefined;
    if (def.type === 'number') {
        if (typeof value !== 'number' || !Number.isFinite(value)) return undefined;
        return clamp(Math.round(value), textSizeRange.min, textSizeRange.max);
    }
    return typeof value === 'boolean' ? value : undefined;
}

function readOverrides() {
    try {
        const raw = localStorage.getItem(STORAGE_KEY);
        if (!raw) return {};
        const parsed = JSON.parse(raw);
        if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return {};
        const clean = {};
        for (const [k, v] of Object.entries(parsed)) {
            const ok = sanitize(k, v);
            if (ok !== undefined) clean[k] = ok;
        }
        return clean;
    } catch (e) {
        return {};
    }
}

function writeOverrides() {
    try {
        if (Object.keys(overrides).length === 0) localStorage.removeItem(STORAGE_KEY);
        else localStorage.setItem(STORAGE_KEY, JSON.stringify(overrides));
    } catch (e) { /* private mode / quota — settings stay for this page only */ }
}

/** Effective value: browser override, else server default, else built-in. */
export function getSetting(key) {
    if (Object.prototype.hasOwnProperty.call(overrides, key)) return overrides[key];
    const fromServer = sanitize(key, serverDefaults[key]);
    if (fromServer !== undefined) return fromServer;
    return byKey.get(key)?.fallback ?? false;
}

/** Reflect every setting onto the document and refresh the menu controls. */
export function applySettings() {
    const root = document.documentElement;
    for (const s of SETTINGS) {
        const value = getSetting(s.key);
        if (s.apply) s.apply(value);
        else root.classList.toggle(s.cls, s.whenFalse ? !value : value);
    }
    syncControls();
}

function syncControls() {
    for (const s of SETTINGS) {
        if (!s.input) continue;
        const input = document.getElementById(s.input);
        if (input) input.checked = getSetting(s.key);
    }

    const pct = getSetting('text_size');
    const label = document.getElementById('set-text-reset');
    if (label) label.textContent = pct + '%';
    const smaller = document.getElementById('set-text-smaller');
    if (smaller) smaller.disabled = pct <= textSizeRange.min;
    const bigger = document.getElementById('set-text-bigger');
    if (bigger) bigger.disabled = pct >= textSizeRange.max;
}

function setSetting(key, value) {
    const clean = sanitize(key, value);
    if (clean === undefined) return;
    overrides[key] = clean;
    afterChange(key, clean);
}

/** Drop the override for one setting, falling back to the server default. */
function clearSetting(key) {
    if (!byKey.has(key)) return;
    delete overrides[key];
    afterChange(key, getSetting(key));
}

function afterChange(key, value) {
    writeOverrides();
    applySettings();
    // High contrast swaps the CSS custom properties the charts are painted
    // from, and text size changes their label metrics; both need a repaint.
    if (key === 'high_contrast' || key === 'text_size') applyTheme();
    document.dispatchEvent(new CustomEvent('kula-settings-changed', { detail: { key, value } }));
}

/** Drop every browser override and fall back to the server defaults. */
export function resetSettings() {
    overrides = {};
    writeOverrides();
    applySettings();
    applyTheme();
    document.dispatchEvent(new CustomEvent('kula-settings-changed', { detail: { key: null, value: null } }));
}

function stepTextSize(direction) {
    const { min, max, step } = textSizeRange;
    const next = clamp(getSetting('text_size') + direction * step, min, max);
    setSetting('text_size', next);
}

/**
 * Adopt the defaults from /api/config. Overrides still win, so this only
 * moves settings the visitor never touched.
 */
export function applyServerSettings(cfg) {
    const range = cfg?.accessibility?.text_size_range;
    if (range && Number.isFinite(range.min) && Number.isFinite(range.max) && range.min < range.max) {
        textSizeRange.min = range.min;
        textSizeRange.max = range.max;
        if (Number.isFinite(range.step) && range.step > 0) textSizeRange.step = range.step;
    }

    const merged = {};
    for (const group of ['appearance', 'accessibility']) {
        const section = cfg?.[group];
        if (!section || typeof section !== 'object') continue;
        for (const s of SETTINGS) {
            if (s.group !== group) continue;
            const value = sanitize(s.key, section[s.key]);
            if (value !== undefined) merged[s.key] = value;
        }
    }
    serverDefaults = merged;

    // A narrower server range can put a previously stored size out of bounds.
    const stored = overrides.text_size;
    if (stored !== undefined) {
        const reclamped = sanitize('text_size', stored);
        if (reclamped !== stored) {
            overrides.text_size = reclamped;
            writeOverrides();
        }
    }

    applySettings();
    applyTheme();
}

/** Wire the header button, the dropdown and the controls. */
export function initSettingsMenu() {
    const btn = document.getElementById('btn-settings');
    const menu = document.getElementById('settings-menu');
    if (!btn || !menu) return;

    const close = () => {
        menu.classList.add('hidden');
        btn.setAttribute('aria-expanded', 'false');
    };

    btn.addEventListener('click', (e) => {
        e.stopPropagation();
        const opening = menu.classList.contains('hidden');
        menu.classList.toggle('hidden', !opening);
        btn.setAttribute('aria-expanded', String(opening));
    });

    // Clicks inside the menu drive its controls; they must not dismiss it.
    menu.addEventListener('click', (e) => e.stopPropagation());
    document.addEventListener('click', close);
    document.addEventListener('keydown', (e) => {
        if (e.key === 'Escape' && !menu.classList.contains('hidden')) {
            close();
            btn.focus();
        }
    });

    for (const s of SETTINGS) {
        if (!s.input) continue;
        const input = document.getElementById(s.input);
        if (input) input.addEventListener('change', () => setSetting(s.key, input.checked));
    }

    document.getElementById('set-text-smaller')?.addEventListener('click', () => stepTextSize(-1));
    document.getElementById('set-text-bigger')?.addEventListener('click', () => stepTextSize(1));
    document.getElementById('set-text-reset')?.addEventListener('click', () => clearSetting('text_size'));
    document.getElementById('btn-settings-reset')?.addEventListener('click', resetSettings);

    syncControls();
}
