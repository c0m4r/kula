/* ============================================================
   ollama.js — AI Assistant panel backed by the local Ollama
   LLM API. Streams responses via Server-Sent Events from
   the /api/ollama/chat backend endpoint.
   ============================================================ */
'use strict';
import { state, escapeHTML } from './state.js';
import { i18n } from './i18n.js';
import { apiUrl } from './url.js';

// ---- Conversation history (max 20 turns kept per session in memory) ----
const MAX_HISTORY = 20;
let isStreaming = false;
let aiPanelOpen = false;
let ollamaModel = '';
let chartObserver = null; // [M2] stored so we can disconnect on re-init

// ---- Sessions ----
// Each session is an independent analysis thread with its own pinned context
// (current-metrics snapshot or a specific chart CSV) and its own message
// history. Sessions live only for the tab's lifetime.
//   Shape: { id, label, kind: 'current' | 'chart', context, history }
let sessions = [];
let activeSessionId = null;
let nextSessionId = 1;

// ---- Model selection ----
let selectedModel = '';     // currently selected model name (sent with each request)
let modelPollTimer = null;  // setTimeout handle for the next models poll
let serviceAvailable = true;
let pendingChartAnalysis = null; // { chartTitle, csvData } queued while service is down

// ---- Init ----

/**
 * initOllama — called after /api/config is fetched.
 * Shows the AI button when ollama is enabled.
 */
export function initOllama(cfg) {
    if (!cfg.ollama_enabled) return;
    ollamaModel = cfg.ollama_model || 'llama3';
    selectedModel = ollamaModel;
    const btn = document.getElementById('btn-ai');
    if (btn) btn.classList.remove('hidden');

    // Seed the model selector with the config default immediately.
    const select = document.getElementById('ai-model-select');
    if (select) {
        select.innerHTML = `<option value="${ollamaModel}">${ollamaModel}</option>`;
        select.addEventListener('change', () => { selectedModel = select.value; });
    }

    // Model polling starts when the panel is opened; see openAIPanel().

    // Wire up the panel controls
    document.getElementById('btn-ai-close')?.addEventListener('click', closeAIPanel);
    document.getElementById('btn-ai-send')?.addEventListener('click', sendAnalysis);
    document.getElementById('ai-input')?.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' && !e.shiftKey) {
            e.preventDefault();
            sendAnalysis();
        }
    });
    // Close the panel from any focus when it's open.
    document.addEventListener('keydown', (e) => {
        if (e.key === 'Escape' && aiPanelOpen) closeAIPanel();
    });
    document.getElementById('btn-ai-clear')?.addEventListener('click', () => {
        if (isStreaming) return;
        const sess = getActiveSession();
        if (!sess || sess.history.length === 0) return;
        if (!confirm(i18n.t('ai_confirm_clear'))) return;
        clearConversation();
    });
    document.getElementById('btn-ai-session-new')?.addEventListener('click', () => {
        createSession({ kind: 'current' });
        fetchSessionContext();
    });
    document.getElementById('btn-ai-session-delete')?.addEventListener('click', () => {
        if (isStreaming) return;
        const sess = getActiveSession();
        if (!sess) return;
        if (!confirm(i18n.t('ai_confirm_delete_session'))) return;
        deleteSession(sess.id);
    });
    document.getElementById('ai-session-select')?.addEventListener('change', (e) => {
        const id = Number(e.target.value);
        if (!Number.isNaN(id)) switchSession(id);
    });
    btn.addEventListener('click', toggleAIPanel);

    wirePanelDragResize();

    // Watch for chart elements to add the Analyze button
    document.querySelectorAll('.chart-card, .gauge-card').forEach(card => attachAIButtonToCard(card));

    // [M2] Disconnect any previous observer before creating a new one
    if (chartObserver) chartObserver.disconnect();
    chartObserver = new MutationObserver((mutations) => {
        mutations.forEach((mutation) => {
            mutation.addedNodes.forEach((node) => {
                if (node.nodeType === Node.ELEMENT_NODE) {
                    if (node.classList.contains('chart-card') || node.classList.contains('gauge-card')) {
                        attachAIButtonToCard(node);
                    } else {
                        node.querySelectorAll('.chart-card, .gauge-card').forEach(card => attachAIButtonToCard(card));
                    }
                }
            });
        });
    });
    const grid = document.getElementById('charts-grid');
    if (grid) chartObserver.observe(grid, { childList: true, subtree: true });
}

// ---- Sessions ----

function getActiveSession() {
    return sessions.find((s) => s.id === activeSessionId) || null;
}

/** Create a new session, make it active, and refresh the UI. */
function createSession({ label, context, kind } = {}) {
    const sess = {
        id: nextSessionId++,
        label: label || 'Current metrics',
        kind: kind || 'current',
        context: context || 'current',
        history: [],
    };
    sessions.push(sess);
    activeSessionId = sess.id;
    renderMessagesForActiveSession();
    renderSessionsBar();
    return sess;
}

/** Switch the active session; repaint the messages pane. */
function switchSession(id) {
    if (!sessions.some((s) => s.id === id)) return;
    activeSessionId = id;
    renderMessagesForActiveSession();
    renderSessionsBar();
}

/** Remove a session; fall through to the newest remaining, or start fresh. */
function deleteSession(id) {
    sessions = sessions.filter((s) => s.id !== id);
    if (sessions.length === 0) {
        createSession({ kind: 'current' });
        fetchSessionContext();
        return;
    }
    if (activeSessionId === id) {
        activeSessionId = sessions[sessions.length - 1].id;
    }
    renderMessagesForActiveSession();
    renderSessionsBar();
}

/** Repopulate the session <select> from the current sessions list. */
function renderSessionsBar() {
    const select = document.getElementById('ai-session-select');
    if (!select) return;
    select.innerHTML = '';
    for (const s of sessions) {
        const opt = document.createElement('option');
        opt.value = String(s.id);
        opt.textContent = (s.kind === 'chart' ? '📊 ' : '💬 ') + s.label;
        if (s.id === activeSessionId) opt.selected = true;
        select.appendChild(opt);
    }
}

/** Paint the messages pane for the active session's history. */
function renderMessagesForActiveSession() {
    const messagesEl = document.getElementById('ai-messages');
    if (!messagesEl) return;
    messagesEl.innerHTML = '';
    const sess = getActiveSession();
    if (!sess) return;
    if (sess.history.length === 0 && sess.kind === 'current') {
        renderWelcomePrompts(messagesEl);
        return;
    }
    for (const m of sess.history) appendMessage(m.role, m.content, false);
}

/** Render a welcome block with example prompts the user can click to send. */
function renderWelcomePrompts(messagesEl) {
    const wrap = document.createElement('div');
    wrap.className = 'ai-welcome';

    const hint = document.createElement('div');
    hint.className = 'ai-welcome-hint';
    hint.textContent = i18n.t('ai_try_prompts');
    wrap.appendChild(hint);

    for (const text of [i18n.t('ai_prompt_status'), i18n.t('ai_prompt_load'), i18n.t('ai_prompt_network')]) {
        const btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'ai-welcome-prompt';
        btn.textContent = text;
        btn.addEventListener('click', () => runExamplePrompt(text));
        wrap.appendChild(btn);
    }
    messagesEl.appendChild(wrap);
}

/** Fill the input with an example prompt and send it. */
function runExamplePrompt(text) {
    if (isStreaming) return;
    const input = document.getElementById('ai-input');
    if (input) input.value = text;
    sendAnalysis();
}

// ---- Session context snapshot ----

/**
 * Fetches the current server metrics as a pre-formatted snapshot string from
 * /api/ollama/context and stores it on the active session. The same string is
 * re-sent on every turn so the model sees a stable baseline.
 * Only populates sessions of kind 'current'; chart sessions have their CSV
 * already pinned from analyzeChartData().
 */
async function fetchSessionContext() {
    const sess = getActiveSession();
    if (!sess || sess.kind !== 'current') return;
    try {
        const headers = {};
        if (state.csrfToken) headers['X-CSRF-Token'] = state.csrfToken;
        const resp = await fetch(apiUrl('/api/ollama/context'), { headers });
        if (!resp.ok) return;
        const data = await resp.json();
        if (data.context) sess.context = data.context;
    } catch {
        // Leave context as the 'current' sentinel; backend calls QueryLatest().
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
    if (!aiPanelOpen) return; // paused while panel is closed
    try {
        const headers = {};
        if (state.csrfToken) headers['X-CSRF-Token'] = state.csrfToken;
        const resp = await fetch(apiUrl('/api/ollama/models'), { headers });
        if (!resp.ok) { setServiceAvailable(false); scheduleModelPoll(5000); return; }
        const data = await resp.json();
        const models = data.models || [];
        if (models.length === 0) { setServiceAvailable(false); scheduleModelPoll(5000); return; }
        setServiceAvailable(true);
        updateModelSelector(models);
        scheduleModelPoll(30000);
    } catch {
        setServiceAvailable(false);
        scheduleModelPoll(5000);
    }
}

function setServiceAvailable(ok) {
    if (serviceAvailable === ok) return;
    serviceAvailable = ok;
    const banner = document.getElementById('ai-status-banner');
    if (banner) banner.classList.toggle('hidden', ok);
    if (ok) {
        if (pendingChartAnalysis) {
            const { chartTitle, csvData } = pendingChartAnalysis;
            pendingChartAnalysis = null;
            analyzeChartData(chartTitle, csvData);
        } else {
            const inputEl = document.getElementById('ai-input');
            if (inputEl?.value.trim()) sendAnalysis();
        }
    }
}

/** Rebuild the model <select> from the given list, preserving the current selection. */
function updateModelSelector(models) {
    const select = document.getElementById('ai-model-select');
    if (!select) return;
    const current = selectedModel || ollamaModel;
    select.innerHTML = '';
    let found = false;
    for (const m of models) {
        const opt = document.createElement('option');
        opt.value = m;
        opt.textContent = m;
        if (m === current) { opt.selected = true; found = true; }
        select.appendChild(opt);
    }
    // If the previously selected model is no longer present, fall back to the first.
    selectedModel = found ? current : models[0];
    if (!found) select.value = selectedModel;
}

function attachAIButtonToCard(card) {
    if (card.querySelector('.btn-ai-chart')) return;

    // Only target those with canvas
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
    btn.className = 'btn-ai-chart';
    btn.title = 'Analyse Graph';
    btn.textContent = '🤖';

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

    btn.onclick = (e) => {
        e.stopPropagation();

        const chartInstance = canvas.id ? Chart.getChart(canvas.id) : null;
        if (!chartInstance) return;

        let titleText = 'Chart';
        if (isGauge) {
            titleText = header.textContent;
        } else {
            const h3 = header.querySelector('h3');
            titleText = h3 ? h3.textContent : 'Chart';
        }

        // [H2] catch unhandled rejection from the async function
        extractAndAnalyzeChart(chartInstance, titleText);
    };
}

function extractAndAnalyzeChart(chart, title) {
    let csv = '';
    const datasets = chart.data.datasets.filter(d => d.data && d.data.length > 0 && !d.hidden);
    if (datasets.length === 0) return;

    // Get time axis points from the longest dataset
    let points = datasets.reduce((prev, current) => (prev.data.length > current.data.length) ? prev : current).data.map(p => ({ x: p.x }));

    // Downsample
    const MAX_POINTS = 50;
    if (points.length > MAX_POINTS) {
        const step = Math.ceil(points.length / MAX_POINTS);
        points = points.filter((_, i) => i % step === 0);
    }

    // Header
    const labels = datasets.map(d => `"${(d.label || 'Value').replace(/"/g, '""')}"`);
    csv += 'Time,' + labels.join(',') + '\n';

    // Rows
    points.forEach(pt => {
        const d = new Date(pt.x);
        const timeStr = `${d.getHours().toString().padStart(2,'0')}:${d.getMinutes().toString().padStart(2,'0')}`;
        const row = [timeStr];
        datasets.forEach(ds => {
            const target = ds.data.find(p => p.x >= pt.x) || ds.data[ds.data.length - 1];
            let val = target && target.y !== undefined ? target.y : 0;
            if (typeof val === 'number') val = val.toFixed(2);
            row.push(val);
        });
        csv += row.join(',') + '\n';
    });

    const chartI18nKey = chart.canvas.closest('.chart-card')?.querySelector('h3[data-i18n]')?.dataset.i18n;
    if (chartI18nKey === 'load_average') {
        const numCores = state.lastSample?.cpu?.num_cores;
        if (numCores) csv = `# System has ${numCores} logical CPU cores (load = cores means fully utilized)\n` + csv;
    }

    // [H2] catch unhandled rejection
    analyzeChartData(title, csv).catch(err => console.error('[AI] chart analysis error:', err));
}

// ---- Panel Toggle ----

function toggleAIPanel() {
    if (aiPanelOpen) {
        closeAIPanel();
    } else {
        openAIPanel();
    }
}

function openAIPanel() {
    const panel = document.getElementById('ai-panel');
    if (!panel) return;
    // First open in this tab lifetime: create the default current-metrics
    // session so the user always has somewhere to type.
    if (sessions.length === 0) {
        createSession({ kind: 'current' });
        fetchSessionContext();
    }
    panel.classList.remove('hidden');
    aiPanelOpen = true;
    document.getElementById('ai-input')?.focus();
    renderSessionsBar();
    // Resume model polling; pollOllamaModels() bails out while aiPanelOpen is false.
    pollOllamaModels();
}

function closeAIPanel() {
    const panel = document.getElementById('ai-panel');
    if (!panel) return;
    panel.classList.add('hidden');
    aiPanelOpen = false;
    clearTimeout(modelPollTimer);
}

// ---- Drag to move, resize grip ----

/**
 * Wires pointer-driven drag-to-move on the panel header and drag-to-resize on
 * a bottom-right grip. Dragging starts free-positioning the panel (overrides
 * the default bottom/right anchoring). Interactive controls inside the header
 * (buttons, selects) keep working — drags that start on them are ignored.
 */
function wirePanelDragResize() {
    const panel = document.getElementById('ai-panel');
    const header = panel?.querySelector('.ai-panel-header');
    const grip = document.getElementById('ai-resize-grip');
    if (!panel || !header || !grip) return;

    header.addEventListener('pointerdown', (e) => {
        if (e.target.closest('button, select, input, textarea')) return;
        e.preventDefault();
        const rect = panel.getBoundingClientRect();
        panel.style.left = rect.left + 'px';
        panel.style.top = rect.top + 'px';
        panel.style.right = 'auto';
        panel.style.bottom = 'auto';
        const startX = e.clientX;
        const startY = e.clientY;
        const startLeft = rect.left;
        const startTop = rect.top;
        header.setPointerCapture(e.pointerId);
        const onMove = (ev) => {
            const newLeft = Math.max(0, Math.min(window.innerWidth - 80, startLeft + ev.clientX - startX));
            const newTop = Math.max(0, Math.min(window.innerHeight - 40, startTop + ev.clientY - startY));
            panel.style.left = newLeft + 'px';
            panel.style.top = newTop + 'px';
        };
        const onUp = () => {
            header.removeEventListener('pointermove', onMove);
            header.removeEventListener('pointerup', onUp);
            try { header.releasePointerCapture(e.pointerId); } catch (_) { /* noop */ }
        };
        header.addEventListener('pointermove', onMove);
        header.addEventListener('pointerup', onUp);
    });

    grip.addEventListener('pointerdown', (e) => {
        e.preventDefault();
        e.stopPropagation();
        const rect = panel.getBoundingClientRect();
        const startX = e.clientX;
        const startY = e.clientY;
        const startW = rect.width;
        const startH = rect.height;
        grip.setPointerCapture(e.pointerId);
        // Release max-height cap so vertical resize isn't clamped.
        panel.style.maxHeight = 'none';
        panel.style.maxWidth = 'none';
        const onMove = (ev) => {
            const w = Math.max(300, Math.min(window.innerWidth, startW + ev.clientX - startX));
            const h = Math.max(220, Math.min(window.innerHeight, startH + ev.clientY - startY));
            panel.style.width = w + 'px';
            panel.style.height = h + 'px';
        };
        const onUp = () => {
            grip.removeEventListener('pointermove', onMove);
            grip.removeEventListener('pointerup', onUp);
            try { grip.releasePointerCapture(e.pointerId); } catch (_) { /* noop */ }
        };
        grip.addEventListener('pointermove', onMove);
        grip.addEventListener('pointerup', onUp);
    });

    window.addEventListener('resize', () => {
        if (!aiPanelOpen) return;
        const rect = panel.getBoundingClientRect();
        if (rect.right > window.innerWidth || rect.bottom > window.innerHeight || rect.left < 0 || rect.top < 0) {
            panel.style.left = '';
            panel.style.top = '';
            panel.style.right = '1.5rem';
            panel.style.bottom = '1.5rem';
        }
    });
}

// ---- Conversation ----

/** Clears only the active session's history; leaves the session itself. */
function clearConversation() {
    const sess = getActiveSession();
    if (!sess) return;
    sess.history = [];
    renderMessagesForActiveSession();
    if (sess.kind === 'current') fetchSessionContext();
}

function appendMessage(role, content, streaming = false) {
    const messages = document.getElementById('ai-messages');
    if (!messages) return null;

    const div = document.createElement('div');
    div.className = role === 'user' ? 'ai-msg ai-msg-user' : 'ai-msg ai-msg-assistant';

    const label = document.createElement('div');
    label.className = 'ai-msg-label';
    label.textContent = role === 'user' ? 'You' : '🤖 ' + (selectedModel || ollamaModel);

    const body = document.createElement('div');
    body.className = 'ai-msg-body';
    if (streaming) body.classList.add('ai-typing');
    body.innerHTML = renderMarkdownLite(content);

    div.appendChild(label);
    div.appendChild(body);
    messages.appendChild(div);
    scrollToBottom(messages);
    return body;
}

/** Scroll to bottom only when the user is already near the bottom. [M8] */
function scrollToBottom(messages) {
    const atBottom = messages.scrollHeight - messages.scrollTop - messages.clientHeight < 60;
    if (atBottom) messages.scrollTop = messages.scrollHeight;
}

/** Light markdown → HTML renderer. */
function renderMarkdownLite(text) {
    let s = escapeHTML(text);

    // Reasoning blocks: <think>...</think>
    s = s.replace(/&lt;think&gt;([\s\S]*?)(&lt;\/think&gt;|$)/g, (_, content) => {
        return `<div class="ai-think">${content}</div>`;
    });

    // Fenced code blocks: ```lang\ncode``` (closing fence optional during streaming)
    s = s.replace(/```(\w*)\n([\s\S]*?)(?:```|$)/g, (_, lang, code) => {
        const trimmed = code.replace(/\n$/, '');
        const cls = lang ? ` class="language-${lang}"` : '';
        return `<pre><code${cls}>${trimmed}</code></pre>`;
    });

    // GFM-style pipe tables:
    //   | h1 | h2 |
    //   | --- | --- |
    //   | a  | b  |
    // Requires a leading "|" on every row and a separator row with dashes.
    // Runs before the horizontal-rule pass so "| --- | --- |" is consumed here.
    s = s.replace(
        /(^|\n)(\|[^\n]+\|[ \t]*\n\|[ \t:|-]+\|[ \t]*\n(?:\|[^\n]*\|[ \t]*(?:\n|$))+)/g,
        (_, pre, block) => {
            const rows = block.trim().split('\n');
            const splitCells = (row) => {
                const trimmed = row.trim().replace(/^\||\|$/g, '');
                return trimmed.split('|').map((c) => c.trim());
            };
            const header = splitCells(rows[0]);
            let html = '<table class="ai-table"><thead><tr>';
            for (const h of header) html += `<th>${h}</th>`;
            html += '</tr></thead><tbody>';
            for (let i = 2; i < rows.length; i++) {
                html += '<tr>';
                for (const c of splitCells(rows[i])) html += `<td>${c}</td>`;
                html += '</tr>';
            }
            html += '</tbody></table>';
            return pre + html;
        },
    );

    // Horizontal rules: ---, ***, ___
    s = s.replace(/(^|\n)([-*_]){3,}(\n|$)/g, '$1<hr>$3');

    // Headings: # H1, ## H2, ### H3
    s = s.replace(/(^|\n)#{1,3} (.+)/g, (_, pre, heading) => `${pre}<strong>${heading}</strong>`);

    // Bold: **text**
    s = s.replace(/\*\*([^*\n]+)\*\*/g, '<strong>$1</strong>');

    // Inline code: `code` (no newlines inside)
    s = s.replace(/`([^`\n]+)`/g, '<code>$1</code>');

    // Newlines → <br>, but preserve newlines inside <pre> and <table> blocks.
    const parts = s.split(/(<pre[\s\S]*?<\/pre>|<table[\s\S]*?<\/table>)/g);
    s = parts.map((part, i) => i % 2 === 1 ? part : part.replace(/\n/g, '<br>')).join('');

    return s;
}

// ---- Shared streaming helper [M4] ----

/**
 * streamChatResponse fetches /api/ollama/chat and streams the SSE response
 * into assistantBody, returning the full accumulated content string.
 * Releases the reader lock even on error. [M1]
 */
async function streamChatResponse({ prompt, messages, context, assistantBody }) {
    const headers = { 'Content-Type': 'application/json' };
    if (state.csrfToken) headers['X-CSRF-Token'] = state.csrfToken;

    const resp = await fetch(apiUrl('/api/ollama/chat'), {
        method: 'POST',
        headers,
        body: JSON.stringify({
            prompt,
            messages: messages.slice(-MAX_HISTORY),
            context,
            lang: i18n.currentLang,
            model: selectedModel || undefined,
        }),
    });

    if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: 'Request failed' }));
        throw new Error(err.error || `HTTP ${resp.status}`);
    }

    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    let fullContent = '';
    let lastEvent = '';

    try { // [M1] ensure reader is always released
        while (true) {
            const { value, done } = await reader.read();
            if (done) break;
            buffer += decoder.decode(value, { stream: true });

            const lines = buffer.split('\n');
            buffer = lines.pop(); // keep incomplete last line

            for (const line of lines) {
                if (line.startsWith('event: ')) {
                    lastEvent = line.slice(7).trim();
                    continue;
                }
                if (!line.startsWith('data: ')) continue;

                if (lastEvent === 'tool_call') {
                    // Show a temporary indicator while the tool executes.
                    if (assistantBody) {
                        assistantBody.innerHTML = renderMarkdownLite(fullContent) +
                            '<span class="ai-tool-call">🔧 Fetching metrics…</span>';
                        const msgs = document.getElementById('ai-messages');
                        if (msgs) scrollToBottom(msgs);
                    }
                    lastEvent = '';
                    continue;
                }
                if (lastEvent === 'done') {
                    lastEvent = '';
                    break;
                }
                if (lastEvent === 'error') {
                    const msg = line.slice(6) || 'stream error';
                    lastEvent = '';
                    throw new Error(msg);
                }
                lastEvent = '';

                const chunk = line.slice(6);
                const decoded = chunk.replace(/\\n/g, '\n');
                fullContent += decoded;
                if (assistantBody) {
                    assistantBody.innerHTML = renderMarkdownLite(fullContent);
                    const msgs = document.getElementById('ai-messages');
                    if (msgs) scrollToBottom(msgs); // [M8]
                }
            }
        }
    } finally {
        reader.releaseLock(); // [M1]
    }

    assistantBody?.classList.remove('ai-typing');
    return fullContent;
}

// ---- Send ----

async function sendAnalysis() {
    if (isStreaming) return;
    if (!serviceAvailable) return;
    const sess = getActiveSession();
    if (!sess) return;

    const inputEl = document.getElementById('ai-input');
    const prompt = (inputEl?.value || '').trim();

    // Clear input
    if (inputEl) inputEl.value = '';

    // Append user message to UI + active session history
    if (prompt) {
        appendMessage('user', prompt);
        sess.history.push({ role: 'user', content: prompt });
        if (sess.history.length > MAX_HISTORY) sess.history = sess.history.slice(-MAX_HISTORY);
    }

    const assistantBody = appendMessage('assistant', '', true);
    isStreaming = true;
    setUIBusy(true);

    try {
        const fullContent = await streamChatResponse({
            prompt,
            messages: sess.history,
            context: sess.context,
            assistantBody,
        });
        if (fullContent) {
            sess.history.push({ role: 'assistant', content: fullContent });
            if (sess.history.length > MAX_HISTORY) sess.history = sess.history.slice(-MAX_HISTORY);
        }
    } catch (err) {
        if (assistantBody) {
            assistantBody.classList.remove('ai-typing');
            assistantBody.innerHTML = `<span class="ai-error">⚠ ${escapeHTML(err.message)}</span>`;
        }
    } finally {
        isStreaming = false;
        setUIBusy(false);
    }
}

function setUIBusy(busy) {
    const btn = document.getElementById('btn-ai-send');
    if (btn) {
        btn.disabled = busy;
        btn.textContent = busy ? '…' : i18n.t('ai_analyse');
    }
}

/**
 * analyzeChartData — opens the AI panel and routes the chart into a session.
 * If a chart session for this title already exists, resume it (no new prompt
 * is sent — the user continues the thread). Otherwise create a fresh chart
 * session and send the initial "Analyse this data" prompt.
 */
export async function analyzeChartData(chartTitle, csvData) {
    if (isStreaming) return; // [H1]

    openAIPanel();

    if (!serviceAvailable) {
        pendingChartAnalysis = { chartTitle, csvData };
        return;
    }

    const existing = sessions.find((s) => s.kind === 'chart' && s.label === chartTitle);
    let sess;
    if (existing) {
        switchSession(existing.id);
        sess = existing;
        sess.context = `chart: ${chartTitle}\n${csvData}`;
    } else {
        sess = createSession({
            label: chartTitle,
            kind: 'chart',
            context: `chart: ${chartTitle}\n${csvData}`,
        });
    }

    const prompt = i18n.t('ai_analyse_prompt').replace('{title}', chartTitle);
    appendMessage('user', prompt);
    sess.history.push({ role: 'user', content: prompt });

    const assistantBody = appendMessage('assistant', '', true);
    isStreaming = true;
    setUIBusy(true);

    try {
        const fullContent = await streamChatResponse({
            prompt,
            messages: sess.history,
            context: sess.context,
            assistantBody,
        });
        if (fullContent) {
            sess.history.push({ role: 'assistant', content: fullContent });
            if (sess.history.length > MAX_HISTORY) sess.history = sess.history.slice(-MAX_HISTORY);
        }
    } catch (err) {
        if (assistantBody) {
            assistantBody.classList.remove('ai-typing');
            assistantBody.innerHTML = `<span class="ai-error">⚠ ${escapeHTML(err.message)}</span>`;
        }
    } finally {
        isStreaming = false;
        setUIBusy(false);
    }
}
