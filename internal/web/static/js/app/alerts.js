/* ============================================================
   alerts.js — Alert evaluation and alert dropdown UI.
   ============================================================ */
'use strict';
import { state, escapeHTML } from './state.js';

export function evaluateAlerts(sample) {
    const alerts = [];
    const numCores = sample.cpu?.num_cores || 1;

    // Clock not synced
    if (sample.sys?.clock_synced === false) {
        alerts.push({
            icon: '⏱',
            title: 'Clock not synchronized',
            detail: 'Source: ' + (sample.sys.clock_source || 'unknown'),
        });
    }

    // Low entropy
    if (sample.sys?.entropy !== undefined && sample.sys.entropy < 256) {
        alerts.push({
            icon: '🎲',
            title: 'Low entropy',
            detail: `Current: ${sample.sys.entropy} (min recommended: 256)`,
        });
    }

    // Load average exceeds core count
    if (sample.lavg?.load1 > numCores) {
        alerts.push({
            icon: '🔥',
            title: 'Load exceeds core count',
            detail: `Load1: ${sample.lavg.load1.toFixed(2)}, Cores: ${numCores}`,
        });
    }

    // CPU usage > 95%
    if (sample.cpu?.total?.usage > 95) {
        alerts.push({
            icon: '🔥',
            title: 'High CPU usage',
            detail: `CPU: ${sample.cpu.total.usage.toFixed(1)}%`,
        });
    }

    // RAM usage > 95%
    if (sample.mem?.used_pct > 95) {
        alerts.push({
            icon: '💾',
            title: 'High memory usage',
            detail: `RAM: ${sample.mem.used_pct.toFixed(1)}%`,
        });
    }

    // SWAP usage > 95%
    if (sample.swap?.used_pct > 95) {
        alerts.push({
            icon: '💾',
            title: 'High swap usage',
            detail: `Swap: ${sample.swap.used_pct.toFixed(1)}%`,
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
        list.innerHTML = '<div class="alert-empty">No active alerts</div>';
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

export function toggleInfoDropdown() {
    state.infoDropdownOpen = !state.infoDropdownOpen;
    const dropdown = document.getElementById('info-dropdown');
    if (state.infoDropdownOpen) {
        dropdown.classList.remove('hidden');
    } else {
        dropdown.classList.add('hidden');
    }
}
