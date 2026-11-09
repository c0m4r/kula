/* ============================================================
   auth.js — Authentication check, config fetch, login/logout.
   ============================================================ */
'use strict';
import { state, storedAggregation } from './state.js';
import { isAggregation } from './history-navigation.js';
import { readPref } from './prefs.js';
import { applyChartBounds, applySpanGaps } from './charts-init.js';
import { updateAllCharts, clearAllChartData, cancelHistoryRequest } from './charts-data.js';
import { connectWS, disconnectWS } from './websocket.js';
import { applyTheme } from './settings.js';
import { applySplitFromConfig } from './split.js';
import { apiUrl } from './api.js';
import { i18n } from './i18n.js';
import { closeSystemInfo } from './system-info.js';

export function checkAuth() {
    fetch(apiUrl('/api/auth/status'))
        .then(r => r.json())
        .then(data => {
            if (data.auth_required && !data.authenticated) {
                requireLogin();
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

// Session expiry and explicit logout share the same cleanup. Invalidate pending
// responses before clearing data so a late request cannot restore the old view.
export function requireLogin() {
    cancelHistoryRequest();
    disconnectWS();
    closeSystemInfo({ useHistory: false });
    state.csrfToken = null;
    document.getElementById('btn-logout')?.classList.add('hidden');
    document.getElementById('login-overlay')?.classList.remove('hidden');
    document.getElementById('dashboard').style.filter = 'blur(8px)';
    const userEl = document.getElementById('login-user');
    if (userEl) userEl.value = '';
    const passEl = document.getElementById('login-pass');
    if (passEl) passEl.value = '';
    document.getElementById('login-error')?.classList.add('hidden');

    state.dataBuffer = [];
    state.historyPointContexts.clear();
    state.liveQueue = [];
    clearAllChartData();
    updateAllCharts();
}

export function fetchConfig() {
    return fetch(apiUrl('/api/config'))
        .then(r => {
            if (!r.ok) throw new Error('Unauthorized');
            return r.json();
        })
        .then(cfg => {
            if (cfg.history) {
                state.collectionIntervalMs = cfg.history.collection_interval_ms || 1000;
                state.retainedRanges = cfg.history.ranges || [];
            }
            if (cfg.join_metrics !== undefined) {
                state.joinMetrics = cfg.join_metrics;
                applySpanGaps();
            }
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
            if (cfg.hostname) {
                const hostnameEl = document.getElementById('hostname');
                if (hostnameEl) hostnameEl.textContent = cfg.hostname;
                document.title = `KULA - ${cfg.hostname}`;
            }
            if (cfg.theme && !readPref('kula_theme')) {
                state.theme = cfg.theme;
                applyTheme();
            }
            if (isAggregation(cfg.aggregation)) state.defaultAggregation = cfg.aggregation;
            if (isAggregation(cfg.aggregation) && !storedAggregation() && !state.aggFromUrl) {
                state.currentAggregation = cfg.aggregation;
                state.suspendedAggregation = null;
                // Update active button state in the UI
                const aggBtns = document.querySelectorAll('#agg-presets-list .time-btn');
                aggBtns.forEach(b => b.classList.remove('active'));
                const activeBtn = document.querySelector(`#agg-presets-list .time-btn[data-agg="${state.currentAggregation}"]`);
                if (activeBtn) activeBtn.classList.add('active');
            }
            if (cfg.graphs) {
                state.configMax = cfg.graphs;
                applyChartBounds();
                if (cfg.graphs.split) {
                    applySplitFromConfig(cfg.graphs.split);
                }
            }
            if (cfg.custom_metrics) {
                state.customMetricsConfig = cfg.custom_metrics;
            }

            // Notify other modules that config is available
            document.dispatchEvent(new CustomEvent('kula-config-ready', { detail: cfg }));

            const versionStr = cfg.show_version === false ? '' : ' v' + (cfg.version || '0.0.0');
            console.log(
                // nosemgrep: unsafe-formatstring -- static console banner, %c are CSS styling directives, not user input
                '%c K U L A %c' + versionStr + ' %c Welcome to your monitoring dashboard! ',
                'background: #0e1f2fff; color: #fff; border-radius: 3px 0 0 3px; padding: 3px 6px; font-weight: bold; font-family: sans-serif;',
                'background: #0b406eff; color: #fff; border-radius: 0 3px 3px 0; padding: 3px ' + (cfg.show_version === false ? '0' : '6px') + '; font-weight: bold; font-family: sans-serif;',
                'color: #000000ff; font-weight: 500; font-family: sans-serif; margin-left: 10px;'
            );
            return cfg;
        })
        .catch(() => { });
}

export function handleLogin(e) {
    e.preventDefault();
    const user = document.getElementById('login-user')?.value;
    const pass = document.getElementById('login-pass')?.value;
    const errorEl = document.getElementById('login-error');

    fetch(apiUrl('/api/login'), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ username: user, password: pass }),
    })
        .then(r => {
            if (!r.ok) throw new LoginError(LOGIN_ERRORS[r.status] || 'login_failed');
            return r.json();
        })
        .then(async data => {
            if (data && data.csrf_token) {
                state.csrfToken = data.csrf_token;
            }
            document.getElementById('login-overlay')?.classList.add('hidden');
            document.getElementById('dashboard').style.filter = '';
            document.getElementById('btn-logout')?.classList.remove('hidden');
            errorEl?.classList.add('hidden');
            const config = await fetchConfig();
            await i18n.refreshAfterAuth(config);
            connectWS();
        })
        .catch(err => {
            if (errorEl) {
                errorEl.textContent = loginText(err instanceof LoginError ? err.key : 'login_failed');
                errorEl.classList.remove('hidden');
            }
        });
}

// The login form's message for a refused sign-in: a wrong password, the
// per-IP rate limit, and every password-hashing slot being busy are
// different answers, and only the first means the credentials were wrong.
const LOGIN_ERRORS = {
    401: 'login_invalid_credentials',
    429: 'login_rate_limited',
    503: 'login_busy',
};

// /api/i18n needs a session, so a first sign-in has no translations yet and
// shows these; a page whose session expired keeps its language.
const LOGIN_ENGLISH = {
    login_invalid_credentials: 'Invalid username or password',
    login_rate_limited: 'Too many sign-in attempts. Try again later.',
    login_busy: 'The server is busy. Try again in a moment.',
    login_failed: 'Sign-in failed. Try again.',
};

function loginText(key) {
    const translated = i18n.t(key);
    return translated !== key ? translated : LOGIN_ENGLISH[key];
}

class LoginError extends Error {
    constructor(key) {
        super(key);
        this.key = key;
    }
}

export function handleLogout() {
    closeSystemInfo({ useHistory: false });
    const headers = {};
    if (state.csrfToken) {
        headers['X-CSRF-Token'] = state.csrfToken;
    }
    fetch(apiUrl('/api/logout'), {
        method: 'POST',
        headers: headers
    })
        .then(() => requireLogin())
        .catch(err => console.error('Logout error:', err));
}
