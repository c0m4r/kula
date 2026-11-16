/* ============================================================
   ollama.js — AI Assistant panel backed by the local Ollama
   LLM API. Streams responses via Server-Sent Events from
   the /api/ollama/chat backend endpoint.
   ============================================================ */
'use strict';
import { state } from './state.js';
import { i18n } from './i18n.js';
import { apiUrl } from './api.js';
import { formatClockTimestamp, formatRangeTimestamp } from './format.js';
import { readJsonPref, readPref, removePref, writeJsonPref, writePref } from './prefs.js';
import {
    appendAnswer, AI_MAX_ANSWER, chatErrorKey, chatRequestBody, createFrameRenderer, createSSEReader,
    modelTurns, parseToolCall,
} from './ai-stream.js';
import { chartCSV } from './ai-chart-csv.js';
import { renderMarkdown } from './ai-markdown.js';

const MODEL_PREF = 'kula_ai_model';
const GEOMETRY_PREF = 'kula_ai_panel';
// A current-metrics session asked a question this long after its snapshot
// fetches a new one first.
const SNAPSHOT_MAX_AGE_MS = 60 * 1000;
// Past this wait the bubble also counts the seconds.
const SLOW_WAIT_MS = 5000;
const INPUT_WARN_AT = 1800;
const LABEL_MAX = 40;
const HELP_URL = 'https://github.com/c0m4r/kula/blob/main/docs/user/10-ai-assistant.md';
const SHEET_QUERY = '(max-width: 480px)';
// get_metrics resources, named by the dashboard's chart titles.
const RESOURCE_KEYS = {
    cpu: 'cpu_usage', memory: 'memory_usage', swap: 'swap_usage', load: 'load_average',
    network: 'network_throughput', disk_io: 'disk_io', disk_space: 'disk_space', gpu: 'gpu_load',
};

let controlsWired = false;
let panelOpen = false;
let configuredModel = '';
let selectedModel = '';
let modelPollTimer = null;
// 'unknown' until the first model listing: 'ok', 'down' (the backend did not
// answer) or 'nomodels' (it answered with none).
let service = 'unknown';
let pendingChartSession = null; // a chart session whose analysis waits for the backend
const noToolModels = new Set(); // models the server found to lack tool support
let returnFocus = null;
let following = true;
let ticker = null;
let el = {};

// ---- Sessions ----
// Each session is an independent thread with its own pinned context (a
// current-metrics snapshot or a chart's CSV), its transcript and at most one
// answer streaming. A stream belongs to its session, not to the pane: the
// pane is repainted from the session, so switching away and back shows the
// answer as far as it got. Sessions live only for the tab's lifetime.
//   { id, kind: 'current' | 'chart', label, chartKey, context, contextAt,
//     contextInfo, messages, stream }
let sessions = [];
let activeSessionId = null;
let nextSessionId = 1;
let nextMessageId = 1;

const t = (key) => i18n.t(key);
const fmt = (key, values) => i18n.format(key, values);

// ---- Init ----

/**
 * initOllama — called each time /api/config is fetched (at startup and after
 * every sign-in). Shows the AI button when ollama is enabled and wires the
 * panel once: wiring it again would run every action twice.
 */
export function initOllama(cfg) {
    if (!cfg.ollama_enabled) return;
    configuredModel = cfg.ollama_model || 'llama3';
    const btn = document.getElementById('btn-ai');
    if (!btn) return;
    btn.classList.remove('hidden');
    if (controlsWired) return;
    controlsWired = true;
    // The model picked in an earlier visit, until a listing shows whether
    // the backend still has it; the server runs a chat for a model it does
    // not list on the configured one.
    selectedModel = readPref(MODEL_PREF) || configuredModel;

    el = {
        panel: document.getElementById('ai-panel'),
        toggle: btn,
        model: document.getElementById('ai-model-select'),
        sessions: document.getElementById('ai-session-select'),
        newSession: document.getElementById('btn-ai-session-new'),
        deleteSession: document.getElementById('btn-ai-session-delete'),
        confirm: document.getElementById('ai-session-confirm'),
        confirmDelete: document.getElementById('btn-ai-delete-confirm'),
        confirmCancel: document.getElementById('btn-ai-delete-cancel'),
        contextBar: document.getElementById('ai-context-bar'),
        contextText: document.getElementById('ai-context-text'),
        contextAction: document.getElementById('btn-ai-context-action'),
        banner: document.getElementById('ai-status-banner'),
        bannerText: document.getElementById('ai-status-text'),
        bannerHelp: document.getElementById('ai-status-help'),
        messages: document.getElementById('ai-messages'),
        latest: document.getElementById('btn-ai-latest'),
        input: document.getElementById('ai-input'),
        send: document.getElementById('btn-ai-send'),
        counter: document.getElementById('ai-input-counter'),
    };
    if (!el.panel || !el.messages || !el.input || !el.send) return;

    // Seed the model selector with the config default immediately.
    if (el.model) {
        const option = document.createElement('option');
        option.value = option.textContent = selectedModel;
        el.model.replaceChildren(option);
        el.model.addEventListener('change', () => {
            selectedModel = el.model.value;
            writePref(MODEL_PREF, selectedModel);
            if (!getActiveSession()?.messages.length) renderPane();
        });
    }
    if (el.bannerHelp) el.bannerHelp.href = HELP_URL;

    btn.addEventListener('click', toggleAIPanel);
    document.getElementById('btn-ai-close')?.addEventListener('click', closeAIPanel);
    el.send.addEventListener('click', () => {
        const sess = getActiveSession();
        if (sess?.stream) sess.stream.controller.abort();
        else submitInput();
    });
    el.input.addEventListener('keydown', (e) => {
        // An Enter that ends an input method's composition is not a send.
        if (e.key !== 'Enter' || e.shiftKey || e.isComposing || e.keyCode === 229) return;
        e.preventDefault();
        if (!getActiveSession()?.stream) submitInput();
    });
    el.input.addEventListener('input', onInputChanged);
    el.panel.addEventListener('keydown', (e) => {
        if (e.key !== 'Escape' || e.defaultPrevented || e.isComposing) return;
        e.preventDefault();
        if (!el.confirm?.classList.contains('hidden')) hideDeleteConfirm(true);
        else closeAIPanel();
    });
    el.newSession?.addEventListener('click', () => {
        hideDeleteConfirm(false);
        createSession({ kind: 'current' });
        refreshSnapshot(getActiveSession());
        el.input.focus();
    });
    el.deleteSession?.addEventListener('click', () => {
        const sess = getActiveSession();
        if (!sess || sess.stream) return;
        if (sess.messages.length === 0) deleteSession(sess.id);
        else showDeleteConfirm();
    });
    el.confirmDelete?.addEventListener('click', () => {
        const sess = getActiveSession();
        hideDeleteConfirm(false);
        if (sess && !sess.stream) deleteSession(sess.id);
        el.deleteSession?.focus();
    });
    el.confirmCancel?.addEventListener('click', () => hideDeleteConfirm(true));
    el.sessions?.addEventListener('change', () => {
        const id = Number(el.sessions.value);
        if (!Number.isNaN(id)) switchSession(id);
    });
    el.contextAction?.addEventListener('click', onContextAction);
    el.messages.addEventListener('click', onMessagesClick);
    el.messages.addEventListener('scroll', () => {
        following = distanceFromBottom() < 60;
        el.latest?.classList.toggle('hidden', following);
    }, { passive: true });
    el.latest?.addEventListener('click', () => {
        following = true;
        scrollToLatest();
    });

    wirePanelDragResize();

    // Strings painted from script follow a language switch.
    new MutationObserver(() => {
        if (!panelOpen) return;
        renderSessionsBar();
        renderPane();
        renderBanner();
    }).observe(document.documentElement, { attributes: true, attributeFilter: ['lang'] });

    // Watch for chart elements to add the Analyze button
    document.querySelectorAll('.chart-card, .gauge-card').forEach(card => attachAIButtonToCard(card));
    const grid = document.getElementById('charts-grid');
    if (grid) {
        new MutationObserver((mutations) => {
            for (const mutation of mutations) {
                for (const node of mutation.addedNodes) {
                    if (node.nodeType !== Node.ELEMENT_NODE) continue;
                    if (node.classList.contains('chart-card') || node.classList.contains('gauge-card')) {
                        attachAIButtonToCard(node);
                    } else {
                        node.querySelectorAll('.chart-card, .gauge-card').forEach(card => attachAIButtonToCard(card));
                    }
                }
            }
        }).observe(grid, { childList: true, subtree: true });
    }
}

// ---- Sessions ----

function getActiveSession() {
    return sessions.find((s) => s.id === activeSessionId) || null;
}

/** Create a new session, make it active, and refresh the UI. */
function createSession({ kind = 'current', label = '', chartKey = '', context = 'current' } = {}) {
    const sess = {
        id: nextSessionId++,
        kind,
        label,
        chartKey,
        context,
        contextAt: null,
        contextInfo: null,
        messages: [],
        stream: null,
    };
    sessions.push(sess);
    activeSessionId = sess.id;
    following = true;
    renderSessionsBar();
    renderPane();
    return sess;
}

/** Switch the active session; repaint the messages pane. */
function switchSession(id) {
    if (!sessions.some((s) => s.id === id)) return;
    hideDeleteConfirm(false);
    activeSessionId = id;
    following = true;
    renderSessionsBar();
    renderPane();
}

/** Remove a session; fall through to the newest remaining, or start fresh. */
function deleteSession(id) {
    const gone = sessions.find((s) => s.id === id);
    gone?.stream?.controller.abort();
    sessions = sessions.filter((s) => s.id !== id);
    if (pendingChartSession === id) pendingChartSession = null;
    if (sessions.length === 0) {
        createSession({ kind: 'current' });
        refreshSnapshot(getActiveSession());
        return;
    }
    if (activeSessionId === id) activeSessionId = sessions[sessions.length - 1].id;
    following = true;
    renderSessionsBar();
    renderPane();
}

function sessionLabel(sess) {
    const name = sess.label || t('ai_session_new');
    const busy = sess.stream && sess.id !== activeSessionId ? ' …' : '';
    // First-strong isolate: an English question keeps its punctuation in
    // place inside a right-to-left page.
    return (sess.kind === 'chart' ? '📊 ' : '💬 ') + '\u2068' + name + '\u2069' + busy;
}

/** Repopulate the session <select> from the current sessions list. */
function renderSessionsBar() {
    if (!el.sessions) return;
    el.sessions.replaceChildren(...sessions.map((s) => {
        const opt = document.createElement('option');
        opt.value = String(s.id);
        opt.textContent = sessionLabel(s);
        opt.selected = s.id === activeSessionId;
        return opt;
    }));
}

function showDeleteConfirm() {
    if (!el.confirm) return;
    el.confirm.classList.remove('hidden');
    el.confirmDelete?.focus();
}

function hideDeleteConfirm(restoreFocus) {
    if (!el.confirm || el.confirm.classList.contains('hidden')) return;
    el.confirm.classList.add('hidden');
    if (restoreFocus) el.deleteSession?.focus();
}

// ---- Rendering ----

/** Paint the messages pane, the context bar and the controls for the active session. */
function renderPane() {
    const sess = getActiveSession();
    el.messages.replaceChildren();
    if (sess) {
        if (sess.messages.length === 0) {
            el.messages.appendChild(sess.kind === 'chart' ? renderChartEmpty() : renderWelcomePrompts());
        } else {
            for (const msg of sess.messages) el.messages.appendChild(buildMessage(msg, sess));
        }
    }
    renderContextBar();
    updateControls();
    if (following) scrollToLatest();
    el.latest?.classList.toggle('hidden', following);
}

/** Example prompts the user can click to send. */
function renderWelcomePrompts() {
    const wrap = document.createElement('div');
    wrap.className = 'ai-welcome';
    const hint = document.createElement('div');
    hint.className = 'ai-welcome-hint';
    hint.textContent = t('ai_try_prompts');
    wrap.appendChild(hint);
    // The history question needs the get_metrics tool.
    const keys = noToolModels.has(selectedModel)
        ? ['ai_prompt_summary', 'ai_prompt_disk']
        : ['ai_prompt_summary', 'ai_prompt_unusual', 'ai_prompt_disk'];
    for (const key of keys) {
        const btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'ai-welcome-prompt';
        btn.dataset.prompt = t(key);
        btn.textContent = t(key);
        wrap.appendChild(btn);
    }
    return wrap;
}

/** A chart session with no messages offers to analyse its chart. */
function renderChartEmpty() {
    const wrap = document.createElement('div');
    wrap.className = 'ai-welcome';
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'ai-welcome-prompt';
    btn.dataset.action = 'analyse-chart';
    btn.textContent = t('ai_analyse_chart');
    wrap.appendChild(btn);
    return wrap;
}

/** One message's element; paintMessage fills it in. */
function buildMessage(msg, sess) {
    const div = document.createElement('div');
    div.className = msg.role === 'user' ? 'ai-msg ai-msg-user' : 'ai-msg ai-msg-assistant';
    div.dataset.msgId = String(msg.id);
    const label = document.createElement('div');
    label.className = 'ai-msg-label';
    div.appendChild(label);
    if (msg.role === 'assistant') {
        const think = document.createElement('details');
        think.className = 'ai-think ai-think-stream hidden';
        const summary = document.createElement('summary');
        const thinkBody = document.createElement('div');
        thinkBody.className = 'ai-think-body';
        think.append(summary, thinkBody);
        // Clicking the summary is the user's choice; repaints then leave it.
        summary.addEventListener('click', () => { msg.thinkOpen = !think.open; });
        div.appendChild(think);
    }
    const body = document.createElement('div');
    body.className = 'ai-msg-body';
    body.dir = 'auto';
    div.appendChild(body);
    if (msg.role === 'assistant') {
        const status = document.createElement('div');
        status.className = 'ai-msg-status';
        const footer = document.createElement('div');
        footer.className = 'ai-msg-footer';
        div.append(status, footer);
    }
    paintMessage(msg, div, sess);
    return div;
}

const markdownLabels = () => ({ copyLabel: t('ai_copy'), reasoningLabel: t('ai_reasoning') });

function paintMessage(msg, node, sess) {
    const label = node.querySelector('.ai-msg-label');
    const body = node.querySelector('.ai-msg-body');
    if (msg.role === 'user') {
        label.textContent = t('ai_you');
        body.textContent = msg.content;
        return;
    }
    const stream = sess.stream?.msg === msg ? sess.stream : null;
    label.textContent = '🤖 ' + (msg.model || selectedModel || configuredModel);

    const think = node.querySelector('.ai-think-stream');
    think.classList.toggle('hidden', !msg.thinking);
    if (msg.thinking) {
        think.querySelector('summary').textContent = t('ai_reasoning');
        think.querySelector('.ai-think-body').textContent = msg.thinking;
        // Open while the model thinks, closed once the answer starts.
        const open = msg.thinkOpen ?? (!!stream && !msg.content);
        if (think.open !== open) think.open = open;
    }

    body.innerHTML = msg.content ? renderMarkdown(msg.content, markdownLabels()) : '';
    body.classList.toggle('hidden', !msg.content && !msg.error && !msg.stopped && stream?.phase !== 'answering');
    body.classList.toggle('ai-typing', stream?.phase === 'answering');
    if (msg.truncated) appendNote(body, 'ai-truncated', t('ai_answer_truncated'));
    if (msg.stopped) appendNote(body, 'ai-stopped', t('ai_stopped'));
    if (msg.error) {
        const err = appendNote(body, 'ai-error', '⚠ ' + t(msg.error));
        err.setAttribute('role', 'alert');
        body.classList.remove('hidden');
    }

    paintStatus(msg, node, stream);

    const footer = node.querySelector('.ai-msg-footer');
    footer.replaceChildren();
    if (stream) return;
    const facts = [];
    if (msg.tools.length) facts.push(fmt('ai_data_used', { data: msg.tools.map(toolLabel).join(', ') }));
    if (msg.notools) facts.push(t('ai_no_tools'));
    for (const text of facts) {
        const fact = document.createElement('div');
        fact.className = 'ai-msg-fact';
        fact.textContent = text;
        footer.appendChild(fact);
    }
    const actions = document.createElement('div');
    actions.className = 'ai-msg-actions';
    if (msg.content) actions.appendChild(actionButton('copy', t('ai_copy')));
    const last = sess.messages[sess.messages.length - 1] === msg;
    if (last && (msg.error || msg.stopped || !msg.content) && msg.prompt) {
        actions.appendChild(actionButton('retry', t('ai_retry')));
    }
    if (actions.childElementCount) footer.appendChild(actions);
}

function appendNote(parent, className, text) {
    const note = document.createElement('div');
    note.className = className;
    note.textContent = text;
    parent.appendChild(note);
    return note;
}

function actionButton(action, text) {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'btn-ai-text';
    btn.dataset.action = action;
    btn.textContent = text;
    return btn;
}

/** The phase line of an answer being written: what it waits for, and for how long. */
function paintStatus(msg, node, stream) {
    const status = node.querySelector('.ai-msg-status');
    if (!status) return;
    let text = '';
    if (stream && stream.phase !== 'answering') {
        if (stream.phase === 'thinking') text = t('ai_thinking');
        else if (stream.phase === 'tool') text = fmt('ai_fetching', { data: toolLabel(msg.tools[msg.tools.length - 1]) });
        else text = fmt('ai_waiting', { model: msg.model || selectedModel || configuredModel });
        const waited = Date.now() - stream.phaseAt;
        if (waited >= SLOW_WAIT_MS) text += ' · ' + fmt('ai_elapsed', { seconds: Math.floor(waited / 1000) });
    }
    status.textContent = text;
    status.classList.toggle('hidden', !text);
}

function toolLabel(call) {
    if (!call?.resource) return t('ai_metrics');
    const name = RESOURCE_KEYS[call.resource] ? t(RESOURCE_KEYS[call.resource]) : call.resource;
    const range = [call.from, call.to].filter(Boolean).join(' → ');
    return range ? `${name} (${range})` : name;
}

/** Repaint msg if its session is the one shown. */
function repaint(sess, msg) {
    if (sess.id !== activeSessionId) return;
    const node = el.messages.querySelector(`[data-msg-id="${msg.id}"]`);
    if (!node) return;
    paintMessage(msg, node, sess);
    if (following) scrollToLatest();
}

function distanceFromBottom() {
    return el.messages.scrollHeight - el.messages.scrollTop - el.messages.clientHeight;
}

function scrollToLatest() {
    el.messages.scrollTop = el.messages.scrollHeight;
    el.latest?.classList.add('hidden');
}

/** The context line under the sessions bar: what data the session's answers rest on. */
function renderContextBar() {
    if (!el.contextBar) return;
    const sess = getActiveSession();
    let text = '';
    let action = '';
    if (sess?.kind === 'current' && sess.contextAt) {
        text = fmt('ai_snapshot', { time: formatClockTimestamp(sess.contextAt, state.timeZone, i18n.currentLang) });
        action = t('ai_refresh');
    } else if (sess?.kind === 'chart' && sess.contextInfo) {
        const { from, to, series } = sess.contextInfo;
        const sameDay = new Date(from).toDateString() === new Date(to).toDateString();
        const show = (ms) => (sameDay ? formatClockTimestamp : formatRangeTimestamp)(ms, state.timeZone, i18n.currentLang);
        text = `${sess.label} · ${show(from)} – ${show(to)} · ` + fmt('ai_series', { count: series });
        action = t('ai_analyse_again');
    }
    if (sess?.note) text = sess.note;
    el.contextBar.classList.toggle('hidden', !text);
    if (el.contextText) el.contextText.textContent = text;
    if (el.contextAction) {
        el.contextAction.textContent = action;
        el.contextAction.classList.toggle('hidden', !action);
        el.contextAction.disabled = !!sess?.stream;
    }
}

/** Send, Stop and the session controls for the active session's state. */
function updateControls() {
    const sess = getActiveSession();
    const busy = !!sess?.stream;
    const blocked = service === 'down' || service === 'nomodels';
    el.send.textContent = busy ? t('ai_stop') : t('ai_send');
    el.send.classList.toggle('is-stop', busy);
    el.send.disabled = !busy && blocked;
    el.send.title = !busy && blocked ? el.bannerText?.textContent || '' : '';
    el.messages.setAttribute('aria-busy', String(busy));
    if (el.deleteSession) el.deleteSession.disabled = busy;
    if (el.contextAction) el.contextAction.disabled = busy;
}

function onInputChanged() {
    const input = el.input;
    input.style.height = 'auto';
    input.style.height = input.scrollHeight + 2 + 'px'; // capped by max-height
    const n = input.value.length;
    if (el.counter) {
        el.counter.classList.toggle('hidden', n < INPUT_WARN_AT);
        el.counter.textContent = `${n} / ${input.maxLength > 0 ? input.maxLength : 2000}`;
    }
    updateControls();
}

// ---- Clicks inside the pane ----

async function onMessagesClick(e) {
    const target = e.target.closest('button');
    if (!target || !el.messages.contains(target)) return;
    const sess = getActiveSession();
    if (target.classList.contains('ai-code-copy')) {
        const code = target.closest('.ai-code')?.querySelector('code');
        if (code) copyText(code.textContent, target);
        return;
    }
    if (target.classList.contains('ai-welcome-prompt') && target.dataset.prompt) {
        if (sess) ask(sess, target.dataset.prompt);
        return;
    }
    const msg = sess?.messages.find((m) => String(m.id) === target.closest('.ai-msg')?.dataset.msgId);
    switch (target.dataset.action) {
    case 'analyse-chart':
        if (sess) runChartAnalysis(sess);
        break;
    case 'copy':
        if (msg) copyText(msg.content, target);
        break;
    case 'retry':
        if (msg && sess && !sess.stream) {
            // Drop the failed turn and ask again.
            const at = sess.messages.indexOf(msg);
            const prompt = msg.prompt;
            sess.messages.splice(at - 1, 2);
            ask(sess, prompt);
        }
        break;
    default:
    }
}

/** Copy text, then say so on the button for a moment. */
async function copyText(text, button) {
    let ok = false;
    try {
        await navigator.clipboard.writeText(text);
        ok = true;
    } catch {
        // No clipboard API (a plain-HTTP page): copy through a selection.
        const area = document.createElement('textarea');
        area.value = text;
        area.setAttribute('readonly', '');
        area.className = 'ai-copy-buffer';
        document.body.appendChild(area);
        area.select();
        try {
            ok = document.execCommand('copy');
        } catch {
            ok = false;
        }
        area.remove();
    }
    const before = button.textContent;
    button.textContent = ok ? t('ai_copied') : t('ai_copy_failed');
    setTimeout(() => { if (button.isConnected) button.textContent = before; }, 1500);
}

// ---- Session context snapshot ----

/**
 * Fetches the current server metrics as a pre-formatted snapshot string from
 * /api/ollama/context and pins it on a current-metrics session, with the
 * time of the sample it was made from. The same string is re-sent on every
 * turn so the model sees a stable baseline; asking more than
 * SNAPSHOT_MAX_AGE_MS later, or Refresh, takes a new one.
 */
async function refreshSnapshot(sess, signal) {
    if (!sess || sess.kind !== 'current') return;
    try {
        const headers = {};
        if (state.csrfToken) headers['X-CSRF-Token'] = state.csrfToken;
        const resp = await fetch(apiUrl('/api/ollama/context'), { headers, signal });
        if (!resp.ok) return;
        const data = await resp.json();
        if (!data.context) return;
        sess.context = data.context;
        const at = Date.parse(data.ts || '');
        sess.contextAt = Number.isFinite(at) ? at : Date.now();
        if (sess.id === activeSessionId) renderContextBar();
    } catch (err) {
        if (err?.name === 'AbortError') throw err;
        // Leave the context as it was; with the 'current' sentinel the
        // server uses its latest sample.
    }
}

function onContextAction() {
    const sess = getActiveSession();
    if (!sess || sess.stream) return;
    if (sess.kind === 'chart') {
        runChartAnalysis(sess);
    } else {
        refreshSnapshot(sess);
    }
}

// ---- Model polling ----

/** Schedule the next call to pollOllamaModels after `delay` ms. */
function scheduleModelPoll(delay) {
    clearTimeout(modelPollTimer);
    modelPollTimer = setTimeout(pollOllamaModels, delay);
}

/**
 * Fetch the available models from /api/ollama/models.
 * Reschedules itself: 30 s on success with models, 5 s on failure / empty list.
 * Polling is paused while the panel is closed; openAIPanel() resumes it.
 */
async function pollOllamaModels() {
    if (!panelOpen) return; // paused while panel is closed
    try {
        const headers = {};
        if (state.csrfToken) headers['X-CSRF-Token'] = state.csrfToken;
        const resp = await fetch(apiUrl('/api/ollama/models'), { headers });
        if (!resp.ok) { setService('down'); scheduleModelPoll(5000); return; }
        const data = await resp.json();
        const models = Array.isArray(data.models) ? data.models : [];
        if (models.length === 0) { setService('nomodels'); scheduleModelPoll(5000); return; }
        updateModelSelector(models);
        setService('ok');
        scheduleModelPoll(30000);
    } catch {
        setService('down');
        scheduleModelPoll(5000);
    }
}

function setService(next) {
    if (service === next) return;
    service = next;
    renderBanner();
    updateControls();
    // A chart analysis the user asked for while the backend was down runs
    // now. Text in the input is never sent on its own: it may be half typed.
    if (next === 'ok' && pendingChartSession !== null) {
        const sess = sessions.find((s) => s.id === pendingChartSession);
        pendingChartSession = null;
        if (sess) runChartAnalysis(sess);
    }
}

function renderBanner() {
    if (!el.banner) return;
    const down = service === 'down' || service === 'nomodels';
    el.banner.classList.toggle('hidden', !down);
    if (el.bannerText) {
        el.bannerText.textContent = service === 'nomodels'
            ? fmt('ai_no_models', { model: configuredModel })
            : t('ai_unavailable');
    }
    if (el.bannerHelp) el.bannerHelp.textContent = t('ai_help');
}

/** Rebuild the model <select>: the stored choice, else the configured model, else the first listed. */
function updateModelSelector(models) {
    if (!el.model) return;
    const pick = [selectedModel, configuredModel].find((m) => m && models.includes(m)) || models[0];
    el.model.replaceChildren(...models.map((m) => {
        const opt = document.createElement('option');
        opt.value = opt.textContent = m;
        return opt;
    }));
    el.model.value = pick;
    selectedModel = pick;
}

// ---- Chart analysis ----

function attachAIButtonToCard(card) {
    if (card.querySelector('.btn-ai-chart')) return;
    const canvas = card.querySelector('canvas');
    if (!canvas) return;

    let header = card.querySelector('.chart-header');
    // Gauges have label instead of header
    let isGauge = false;
    if (!header) {
        header = card.querySelector('.gauge-label');
        isGauge = true;
        if (!header) return;
    }

    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'btn-ai-chart';
    btn.dataset.i18nTitle = 'ai_analyse_graph';
    btn.dataset.i18nAriaLabel = 'ai_analyse_graph';
    btn.title = t('ai_analyse_graph');
    btn.setAttribute('aria-label', t('ai_analyse_graph'));
    btn.textContent = '✨';

    if (isGauge) {
        btn.classList.add('btn-ai-chart--gauge');
        card.style.position = 'relative';
        card.appendChild(btn);
    } else {
        let rightDiv = header.querySelector('.chart-header-right');
        if (!rightDiv) {
            rightDiv = document.createElement('div');
            rightDiv.className = 'chart-header-right';
            header.style.display = 'flex';
            header.appendChild(rightDiv);
        }
        rightDiv.appendChild(btn);
    }

    btn.addEventListener('click', (e) => {
        e.stopPropagation();
        if (!canvas.id) return;
        const title = (isGauge ? header.textContent : header.querySelector('h3')?.textContent)?.trim() || t('chart');
        openChartSession(canvas.id, title, btn);
    });
}

/**
 * openChartSession — opens the panel on the chart's session. A chart that
 * has none gets one and is analysed; one that has a conversation is only
 * shown, with "Analyse again" offered in its context bar.
 */
function openChartSession(canvasId, title, opener) {
    openAIPanel(opener);
    let sess = sessions.find((s) => s.kind === 'chart' && s.chartKey === canvasId);
    if (sess) {
        switchSession(sess.id);
        if (sess.messages.length || sess.stream) return;
    } else {
        sess = createSession({ kind: 'chart', label: title, chartKey: canvasId, context: '' });
    }
    runChartAnalysis(sess);
}

/** Pin the chart's visible data on its session and ask for an analysis. */
function runChartAnalysis(sess) {
    if (sess.stream) return;
    sess.note = '';
    if (service === 'down' || service === 'nomodels') {
        pendingChartSession = sess.id;
        renderContextBar();
        return;
    }
    const chart = globalThis.Chart?.getChart?.(sess.chartKey);
    const card = chart?.canvas?.closest('.chart-card, .gauge-card');
    const title = (card?.querySelector('.chart-header h3, .gauge-label')?.textContent || '').trim() || sess.label;
    const data = chart ? chartData(chart) : null;
    if (!data) {
        sess.note = t('ai_chart_no_data');
        renderContextBar();
        return;
    }
    sess.label = title;
    sess.context = `chart: ${title}\n${data.csv}`;
    sess.contextInfo = { from: data.from, to: data.to, series: data.series };
    sess.contextAt = Date.now();
    renderSessionsBar();
    ask(sess, fmt('ai_analyse_prompt', { title }));
}

/** The CSV of a chart's series the user can see: hidden ones, by legend or option, are left out. */
function chartData(chart) {
    const series = [];
    chart.data.datasets.forEach((ds, i) => {
        if (chart.isDatasetVisible(i) && ds.data?.length) series.push({ label: ds.label, points: ds.data });
    });
    const notes = [];
    const unit = chart.options?.scales?.y?.title?.text;
    if (typeof unit === 'string' && unit) notes.push(`Values in ${unit}`);
    const key = chart.canvas.closest('.chart-card')?.querySelector('h3[data-i18n]')?.dataset.i18n;
    const cores = state.lastSample?.cpu?.num_cores;
    if (key === 'load_average' && cores) notes.push(`System has ${cores} logical CPU cores (load = cores means fully utilized)`);
    return chartCSV(series, { timeZone: state.timeZone, notes });
}

// ---- Panel Toggle ----

function toggleAIPanel() {
    if (panelOpen) closeAIPanel();
    else openAIPanel(el.toggle);
}

function openAIPanel(opener) {
    if (!el.panel) return;
    // First open in this tab lifetime: create the default current-metrics
    // session so the user always has somewhere to type.
    if (sessions.length === 0) {
        createSession({ kind: 'current' });
        refreshSnapshot(getActiveSession());
    }
    if (!panelOpen) {
        const active = opener || document.activeElement;
        returnFocus = active && !el.panel.contains(active) ? active : el.toggle;
        restoreGeometry();
    }
    el.panel.classList.remove('hidden');
    panelOpen = true;
    el.toggle?.setAttribute('aria-expanded', 'true');
    renderSessionsBar();
    renderBanner();
    renderPane();
    el.input.focus();
    // Resume model polling; pollOllamaModels() bails out while the panel is closed.
    pollOllamaModels();
}

/** Close the panel. A running answer goes on, and is there when it opens again. */
function closeAIPanel() {
    if (!el.panel) return;
    const hadFocus = el.panel.contains(document.activeElement);
    el.panel.classList.add('hidden');
    panelOpen = false;
    el.toggle?.setAttribute('aria-expanded', 'false');
    clearTimeout(modelPollTimer);
    if (hadFocus) (returnFocus?.isConnected ? returnFocus : el.toggle)?.focus();
}

// ---- Drag to move, resize grip ----

const sheetLayout = () => window.matchMedia?.(SHEET_QUERY).matches;

function saveGeometry() {
    const s = el.panel.style;
    writeJsonPref(GEOMETRY_PREF, { left: s.left, top: s.top, width: s.width, height: s.height });
}

/** Reapply a stored position and size that still fits the window. */
function restoreGeometry() {
    if (sheetLayout()) return;
    const g = readJsonPref(GEOMETRY_PREF, null);
    if (!g || typeof g !== 'object') return;
    const px = (v) => (typeof v === 'string' && /^\d+(\.\d+)?px$/.test(v) ? parseFloat(v) : null);
    const [left, top, width, height] = [px(g.left), px(g.top), px(g.width), px(g.height)];
    const s = el.panel.style;
    if (width !== null && width <= window.innerWidth) { s.width = g.width; s.maxWidth = 'none'; }
    if (height !== null && height <= window.innerHeight) { s.height = g.height; s.maxHeight = 'none'; }
    if (left !== null && top !== null && left < window.innerWidth - 80 && top < window.innerHeight - 40) {
        Object.assign(s, { left: g.left, top: g.top, right: 'auto', bottom: 'auto' });
    }
}

function resetGeometry() {
    Object.assign(el.panel.style, { left: '', top: '', right: '', bottom: '', width: '', height: '', maxWidth: '', maxHeight: '' });
}

/**
 * Wires pointer-driven drag-to-move on the panel header and drag-to-resize on
 * a bottom-right grip. Dragging starts free-positioning the panel (overrides
 * the default bottom/right anchoring). Interactive controls inside the header
 * (buttons, selects) keep working — drags that start on them are ignored. In
 * the phone layout the panel is a bottom sheet: neither moves it.
 */
function wirePanelDragResize() {
    const panel = el.panel;
    const header = panel?.querySelector('.ai-panel-header');
    const grip = document.getElementById('ai-resize-grip');
    if (!panel || !header || !grip) return;

    header.addEventListener('pointerdown', (e) => {
        if (sheetLayout() || e.target.closest('button, select, input, textarea, a')) return;
        e.preventDefault();
        const rect = panel.getBoundingClientRect();
        Object.assign(panel.style, { left: rect.left + 'px', top: rect.top + 'px', right: 'auto', bottom: 'auto' });
        const startX = e.clientX;
        const startY = e.clientY;
        header.setPointerCapture(e.pointerId);
        const onMove = (ev) => {
            panel.style.left = Math.max(0, Math.min(window.innerWidth - 80, rect.left + ev.clientX - startX)) + 'px';
            panel.style.top = Math.max(0, Math.min(window.innerHeight - 40, rect.top + ev.clientY - startY)) + 'px';
        };
        const onUp = () => {
            header.removeEventListener('pointermove', onMove);
            header.removeEventListener('pointerup', onUp);
            try { header.releasePointerCapture(e.pointerId); } catch (_) { /* noop */ }
            saveGeometry();
        };
        header.addEventListener('pointermove', onMove);
        header.addEventListener('pointerup', onUp);
    });

    grip.addEventListener('pointerdown', (e) => {
        if (sheetLayout()) return;
        e.preventDefault();
        e.stopPropagation();
        const rect = panel.getBoundingClientRect();
        const startX = e.clientX;
        const startY = e.clientY;
        grip.setPointerCapture(e.pointerId);
        // Pin the top-left corner, so the grip follows the pointer, and
        // release the max-size caps so the resize isn't clamped.
        Object.assign(panel.style, {
            left: rect.left + 'px', top: rect.top + 'px', right: 'auto', bottom: 'auto', maxHeight: 'none', maxWidth: 'none',
        });
        const onMove = (ev) => {
            panel.style.width = Math.max(300, Math.min(window.innerWidth - rect.left, rect.width + ev.clientX - startX)) + 'px';
            panel.style.height = Math.max(220, Math.min(window.innerHeight - rect.top, rect.height + ev.clientY - startY)) + 'px';
        };
        const onUp = () => {
            grip.removeEventListener('pointermove', onMove);
            grip.removeEventListener('pointerup', onUp);
            try { grip.releasePointerCapture(e.pointerId); } catch (_) { /* noop */ }
            saveGeometry();
        };
        grip.addEventListener('pointermove', onMove);
        grip.addEventListener('pointerup', onUp);
    });

    window.addEventListener('resize', () => {
        if (!panelOpen) return;
        if (sheetLayout()) {
            resetGeometry();
            return;
        }
        const rect = panel.getBoundingClientRect();
        if (rect.right > window.innerWidth || rect.bottom > window.innerHeight || rect.left < 0 || rect.top < 0) {
            resetGeometry();
            removePref(GEOMETRY_PREF);
        }
    });
}

// ---- Asking ----

function submitInput() {
    const sess = getActiveSession();
    const prompt = el.input.value.trim();
    // An empty input sends nothing; the summary is an example prompt.
    if (!sess || sess.stream || !prompt) return;
    if (service === 'down' || service === 'nomodels') return;
    el.input.value = '';
    onInputChanged();
    ask(sess, prompt);
}

class ChatError extends Error {
    constructor(key) {
        super(key);
        this.key = key;
    }
}

/**
 * ask puts prompt and an answer to come into sess, and streams the answer
 * into the session. The pane shows it while the session is the active one;
 * switching away and back repaints it from the session. Stop aborts it,
 * keeping what arrived.
 */
async function ask(sess, prompt) {
    if (sess.stream || !prompt) return;
    if (sess.kind === 'current' && !sess.label) {
        sess.label = [...prompt].length > LABEL_MAX ? [...prompt].slice(0, LABEL_MAX - 1).join('') + '…' : prompt;
    }
    // What the model is sent: the answered turns so far, then this question.
    const history = [...modelTurns(sess.messages), { role: 'user', content: prompt }];
    const msg = {
        id: nextMessageId++, role: 'assistant', prompt, content: '', thinking: '', truncated: false,
        model: selectedModel || configuredModel, tools: [], notools: false, error: '', stopped: false, thinkOpen: null,
    };
    sess.messages.push({ id: nextMessageId++, role: 'user', content: prompt }, msg);
    const stream = {
        msg, phase: 'waiting', phaseAt: Date.now(), controller: new AbortController(),
        view: createFrameRenderer(() => repaint(sess, msg)),
    };
    sess.stream = stream;
    if (sess.id === activeSessionId) {
        following = true; // the user's own question is always brought into view
        renderPane();
    }
    renderSessionsBar();
    startTicker();

    try {
        if (sess.kind === 'current' && (!sess.contextAt || Date.now() - sess.contextAt > SNAPSHOT_MAX_AGE_MS)) {
            await refreshSnapshot(sess, stream.controller.signal);
        }
        await streamChat(sess, stream, prompt, history);
    } catch (err) {
        if (err?.name === 'AbortError') msg.stopped = true;
        else msg.error = err instanceof ChatError ? err.key : chatErrorKey({ network: true });
    } finally {
        stream.view.cancel();
        sess.stream = null;
        if (!msg.content && !msg.error && !msg.stopped) msg.error = 'ai_error_empty';
        if (sess.id === activeSessionId) {
            repaint(sess, msg);
            renderContextBar();
            updateControls();
        }
        renderSessionsBar();
    }
}

/** POST the chat and read its event stream into stream.msg. */
async function streamChat(sess, stream, prompt, history) {
    const { msg, controller } = stream;
    const headers = { 'Content-Type': 'application/json' };
    if (state.csrfToken) headers['X-CSRF-Token'] = state.csrfToken;
    let resp;
    try {
        resp = await fetch(apiUrl('/api/ollama/chat'), {
            method: 'POST',
            headers,
            signal: controller.signal,
            body: chatRequestBody({
                prompt,
                history,
                context: sess.context,
                lang: i18n.currentLang,
                model: selectedModel || undefined,
            }),
        });
    } catch (err) {
        if (err?.name === 'AbortError') throw err;
        throw new ChatError(chatErrorKey({ network: true }));
    }
    if (!resp.ok) {
        const body = await resp.json().catch(() => ({}));
        throw new ChatError(chatErrorKey({ status: resp.status, code: body?.code }));
    }

    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    const sse = createSSEReader();
    const setPhase = (phase) => {
        if (stream.phase === phase) return;
        stream.phase = phase;
        stream.phaseAt = Date.now();
    };
    try {
        read: while (true) {
            const { value, done } = await reader.read();
            if (done) break;
            for (const { event, data } of sse.push(decoder.decode(value, { stream: true }))) {
                if (typeof data !== 'string') continue;
                switch (event) {
                case 'done':
                    break read;
                case 'error':
                    throw new ChatError(chatErrorKey({ stream: true }));
                case 'model':
                    if (data) msg.model = data;
                    break;
                case 'notools':
                    msg.notools = true;
                    noToolModels.add(msg.model);
                    break;
                case 'tool_call':
                    msg.tools.push(parseToolCall(data));
                    setPhase('tool');
                    break;
                case 'thinking':
                    msg.thinking = appendAnswer(msg.thinking, data, AI_MAX_ANSWER).answer;
                    if (!msg.content) setPhase('thinking');
                    break;
                case 'message':
                    setPhase('answering');
                    ({ answer: msg.content, truncated: msg.truncated } = appendAnswer(msg.content, data));
                    if (msg.truncated) {
                        // Nothing more will be shown: close the stream, which also
                        // stops the server reading the backend.
                        await reader.cancel().catch(() => {});
                        break read;
                    }
                    break;
                default:
                    continue;
                }
                stream.view.schedule();
            }
        }
    } finally {
        reader.releaseLock();
    }
}

/** Counts the seconds of every visible wait, once a second, while any answer streams. */
function startTicker() {
    if (ticker) return;
    ticker = setInterval(() => {
        const streaming = sessions.filter((s) => s.stream);
        if (streaming.length === 0) {
            clearInterval(ticker);
            ticker = null;
            return;
        }
        const sess = getActiveSession();
        if (!sess?.stream || sess.stream.phase === 'answering') return;
        const node = el.messages.querySelector(`[data-msg-id="${sess.stream.msg.id}"]`);
        if (node) paintStatus(sess.stream.msg, node, sess.stream);
    }, 1000);
}
