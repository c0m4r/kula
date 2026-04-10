/* ============================================================
   auth.js — Authentication check, config fetch, login/logout.
   ============================================================ */
'use strict';
import { state } from './state.js';
import { initCharts } from './charts-init.js';
import { updateAllCharts, clearAllChartData } from './charts-data.js';
import { connectWS, updateConnectionStatus } from './websocket.js';
import { applyTheme } from './theme.js';
import { applySplitFromConfig } from './split.js';

export function checkAuth() {
    fetch('/api/auth/status')
        .then(r => r.json())
        .then(data => {
            if (data.auth_required && !data.authenticated) {
                document.getElementById('login-overlay')?.classList.remove('hidden');
                document.getElementById('dashboard').style.filter = 'blur(8px)';
                document.getElementById('btn-logout')?.classList.add('hidden');
            } else {
                document.getElementById('login-overlay')?.classList.add('hidden');
                document.getElementById('dashboard').style.filter = '';
                if (data.auth_required) {
                    document.getElementById('btn-logout')?.classList.remove('hidden');
                }
                if (data.csrf_token) {
                    state.csrfToken = data.csrf_token;
                }
                fetchConfig().finally(() => {
                    connectWS();
                });
            }
        })
        .catch(() => {
            fetchConfig().finally(() => {
                connectWS();
            });
        }); // If auth check fails, try connecting anyway
}

export function fetchConfig() {
    return fetch('/api/config')
        .then(r => {
            if (!r.ok) throw new Error('Unauthorized');
            return r.json();
        })
        .then(cfg => {
            if (cfg.join_metrics !== undefined) state.joinMetrics = cfg.join_metrics;
            if (cfg.version) {
                const versionEl = document.getElementById('kula-version');
                if (versionEl) versionEl.textContent = 'v' + cfg.version;
            }
            if (cfg.show_version === false) {
                ['kula-version', 'footer-sep'].forEach(id => {
                    const el = document.getElementById(id);
                    if (el) el.classList.add('hidden');
                });
            }
            if (cfg.show_system_info === false) {
                ['row-os', 'row-kernel', 'row-arch'].forEach(id => {
                    const el = document.getElementById(id);
                    if (el) el.classList.add('hidden');
                });
            }
            if (cfg.os) {
                const osEl = document.getElementById('sys-os');
                if (osEl) osEl.textContent = cfg.os;
            }
            if (cfg.kernel) {
                const kernelEl = document.getElementById('sys-kernel');
                if (kernelEl) kernelEl.textContent = cfg.kernel;
            }
            if (cfg.arch) {
                const archEl = document.getElementById('sys-arch');
                if (archEl) archEl.textContent = cfg.arch;
            }
            if (cfg.hostname) {
                const hostnameEl = document.getElementById('hostname');
                if (hostnameEl) hostnameEl.textContent = cfg.hostname;
                document.title = `KULA - ${cfg.hostname}`;
            }
            if (cfg.theme && !localStorage.getItem('kula_theme')) {
                state.theme = cfg.theme;
                applyTheme();
            }
            if (cfg.aggregation && !localStorage.getItem('kula_aggregation')) {
                state.currentAggregation = cfg.aggregation;
                // Update active button state in the UI
                const aggBtns = document.querySelectorAll('#agg-presets-list .time-btn');
                aggBtns.forEach(b => b.classList.remove('active'));
                const activeBtn = document.querySelector(`#agg-presets-list .time-btn[data-agg="${state.currentAggregation}"]`);
                if (activeBtn) activeBtn.classList.add('active');
            }
            if (cfg.graphs) {
                state.configMax = cfg.graphs;
                initCharts(); // reload boundaries immediately on bootstrap/login
                if (cfg.graphs.split) {
                    applySplitFromConfig(cfg.graphs.split);
                }
            }
            if (cfg.custom_metrics) {
                state.customMetricsConfig = cfg.custom_metrics;
            }

            const versionStr = cfg.show_version === false ? '' : ' v' + (cfg.version || '0.0.0');
            console.log(
                '%c K U L A %c' + versionStr + ' %c Welcome to your monitoring dashboard! ',
                'background: #0e1f2fff; color: #fff; border-radius: 3px 0 0 3px; padding: 3px 6px; font-weight: bold; font-family: sans-serif;',
                'background: #0b406eff; color: #fff; border-radius: 0 3px 3px 0; padding: 3px ' + (cfg.show_version === false ? '0' : '6px') + '; font-weight: bold; font-family: sans-serif;',
                'color: #000000ff; font-weight: 500; font-family: sans-serif; margin-left: 10px;'
            );
        })
        .catch(() => { });
}

export function handleLogin(e) {
    e.preventDefault();
    const user = document.getElementById('login-user')?.value;
    const pass = document.getElementById('login-pass')?.value;
    const errorEl = document.getElementById('login-error');

    fetch('/api/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ username: user, password: pass }),
    })
        .then(r => {
            if (!r.ok) throw new Error('Invalid credentials');
            return r.json();
        })
        .then(data => {
            if (data && data.csrf_token) {
                state.csrfToken = data.csrf_token;
            }
            document.getElementById('login-overlay')?.classList.add('hidden');
            document.getElementById('dashboard').style.filter = '';
            document.getElementById('btn-logout')?.classList.remove('hidden');
            errorEl?.classList.add('hidden');
            fetchConfig();
            connectWS();
        })
        .catch(err => {
            if (errorEl) {
                errorEl.textContent = err.message;
                errorEl.classList.remove('hidden');
            }
        });
}

export function handleLogout() {
    const headers = {};
    if (state.csrfToken) {
        headers['X-CSRF-Token'] = state.csrfToken;
    }
    fetch('/api/logout', { 
        method: 'POST',
        headers: headers
    })
        .then(() => {
            if (state.ws) {
                state.ws.close();
            }
            document.getElementById('btn-logout')?.classList.add('hidden');
            document.getElementById('login-overlay')?.classList.remove('hidden');
            document.getElementById('dashboard').style.filter = 'blur(8px)';
            const userEl = document.getElementById('login-user');
            if (userEl) userEl.value = '';
            const passEl = document.getElementById('login-pass');
            if (passEl) passEl.value = '';
            document.getElementById('login-error')?.classList.add('hidden');

            // Clear state
            state.dataBuffer = [];
            state.liveQueue = [];
            clearAllChartData();
            updateAllCharts();
            updateConnectionStatus(false);
        })
        .catch(err => console.error('Logout error:', err));
}
