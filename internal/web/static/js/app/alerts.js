/* ============================================================
   alerts.js — Alert evaluation and alert dropdown UI.
   ============================================================ */
'use strict';
import { state, escapeHTML } from './state.js';
import { i18n } from './i18n.js';

export function evaluateAlerts(sample) {
    const alerts = [];
    const numCores = sample.cpu?.num_cores || 1;
    // Alerts are evaluated with every sample, so a language change reaches
    // them with the next one.

    // Clock not synced
    if (sample.sys?.clock_synced === false) {
        alerts.push({
            icon: '⏱',
            title: i18n.format('alert_clock_unsynced'),
            detail: i18n.format('alert_clock_source', { source: sample.sys.clock_source || i18n.t('alert_clock_source_unknown') }),
        });
    }

    // Low entropy
    if (sample.sys?.entropy !== undefined && sample.sys.entropy < 256) {
        alerts.push({
            icon: '🎲',
            title: i18n.format('alert_low_entropy'),
            detail: i18n.format('alert_entropy_detail', { value: sample.sys.entropy, min: 256 }),
        });
    }

    // Load average exceeds core count
    if (sample.lavg?.load1 > numCores) {
        alerts.push({
            icon: '🔥',
            title: i18n.format('alert_load_high'),
            detail: i18n.format('alert_load_detail', { load: sample.lavg.load1.toFixed(2), cores: numCores }),
        });
    }

    // CPU usage > 95%
    if (sample.cpu?.total?.usage > 95) {
        alerts.push({
            icon: '🔥',
            title: i18n.format('alert_cpu_high'),
            detail: i18n.format('alert_cpu_detail', { pct: sample.cpu.total.usage.toFixed(1) }),
        });
    }

    // RAM usage > 95%
    if (sample.mem?.used_pct > 95) {
        alerts.push({
            icon: '💾',
            title: i18n.format('alert_memory_high'),
            detail: i18n.format('alert_memory_detail', { pct: sample.mem.used_pct.toFixed(1) }),
        });
    }

    // SWAP usage > 95%
    if (sample.swap?.used_pct > 95) {
        alerts.push({
            icon: '💾',
            title: i18n.format('alert_swap_high'),
            detail: i18n.format('alert_swap_detail', { pct: sample.swap.used_pct.toFixed(1) }),
        });
    }

    state.alerts = alerts;
    updateAlertUI();
}

function updateAlertUI() {
    const badge = document.getElementById('alert-badge');
    const btn = document.getElementById('btn-alerts');
    const list = document.getElementById('alert-list');

    if (state.alerts.length > 0) {
        badge.textContent = state.alerts.length;
        badge.classList.remove('hidden');
        btn.classList.add('has-alerts');
        btn.classList.remove('no-alerts');
    } else {
        badge.classList.add('hidden');
        btn.classList.remove('has-alerts');
        btn.classList.add('no-alerts');
    }

    // Render alert items
    if (state.alerts.length === 0) {
        // Keep the key, so a later language change retranslates it too.
        const empty = document.createElement('div');
        empty.className = 'alert-empty';
        empty.dataset.i18n = 'no_active_alerts';
        empty.textContent = i18n.t('no_active_alerts');
        list.replaceChildren(empty);
    } else {
        list.innerHTML = state.alerts.map(a => `
            <div class="alert-item">
                <span class="alert-icon">${escapeHTML(a.icon)}</span>
                <div class="alert-item-body">
                    <div class="alert-item-title">${escapeHTML(a.title)}</div>
                    <div class="alert-item-detail">${escapeHTML(a.detail)}</div>
                </div>
            </div>
        `).join('');
    }
}

export function toggleAlertDropdown() {
    state.alertDropdownOpen = !state.alertDropdownOpen;
    const dropdown = document.getElementById('alert-dropdown');
    if (state.alertDropdownOpen) {
        dropdown.classList.remove('hidden');
    } else {
        dropdown.classList.add('hidden');
    }
}
