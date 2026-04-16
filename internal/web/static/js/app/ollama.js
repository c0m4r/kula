/* ============================================================
   ollama.js — AI Assistant panel backed by the local Ollama
   LLM API. Streams responses via Server-Sent Events from
   the /api/ollama/chat backend endpoint.
   ============================================================ */
'use strict';
import { state, escapeHTML } from './state.js';
import { i18n } from './i18n.js';

// ---- Conversation history (max 20 turns kept in memory) ----
const MAX_HISTORY = 20;
let conversationHistory = [];
let isStreaming = false;
let aiPanelOpen = false;
let ollamaModel = '';
let chartObserver = null; // [M2] stored so we can disconnect on re-init

// ---- Init ----

/**
 * initOllama — called after /api/config is fetched.
 * Shows the AI button when ollama is enabled.
 */
export function initOllama(cfg) {
    if (!cfg.ollama_enabled) return;
    ollamaModel = cfg.ollama_model || 'llama3';
    const btn = document.getElementById('btn-ai');
    if (btn) btn.classList.remove('hidden');

    // Wire up the panel controls
    document.getElementById('btn-ai-close')?.addEventListener('click', closeAIPanel);
    document.getElementById('btn-ai-send')?.addEventListener('click', sendAnalysis);
    document.getElementById('ai-input')?.addEventListener('keydown', (e) => {
        if (e.key === 'Escape') { closeAIPanel(); return; } // [L2]
        if (e.key === 'Enter' && !e.shiftKey) {
            e.preventDefault();
            sendAnalysis();
        }
    });
    document.getElementById('btn-ai-clear')?.addEventListener('click', clearConversation);
    btn.addEventListener('click', toggleAIPanel);

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
    panel.classList.remove('hidden');
    aiPanelOpen = true;
    document.getElementById('ai-input')?.focus();

    // Show model name in header
    const modelLabel = document.getElementById('ai-model-label');
    if (modelLabel) modelLabel.textContent = ollamaModel;
}

function closeAIPanel() {
    const panel = document.getElementById('ai-panel');
    if (!panel) return;
    panel.classList.add('hidden');
    aiPanelOpen = false;
}

// ---- Conversation ----

function clearConversation() {
    conversationHistory = [];
    const messages = document.getElementById('ai-messages');
    if (messages) messages.innerHTML = '';
}

function appendMessage(role, content, streaming = false) {
    const messages = document.getElementById('ai-messages');
    if (!messages) return null;

    const div = document.createElement('div');
    div.className = role === 'user' ? 'ai-msg ai-msg-user' : 'ai-msg ai-msg-assistant';

    const label = document.createElement('div');
    label.className = 'ai-msg-label';
    label.textContent = role === 'user' ? 'You' : '🤖 ' + ollamaModel;

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
    s = s.replace(/```(\w*)\n([\s\S]*?)(?:```|$)/g, (match, lang, code) => {
        const trimmed = code.replace(/\n$/, '');
        const cls = lang ? ` class="language-${lang}"` : '';
        return `<pre><code${cls}>${trimmed}</code></pre>`;
    });

    // Horizontal rules: ---, ***, ___
    s = s.replace(/(^|\n)([-*_]){3,}(\n|$)/g, '$1<hr>$3');

    // Headings: # H1, ## H2, ### H3
    s = s.replace(/(^|\n)#{1,3} (.+)/g, (_, pre, heading) => `${pre}<strong>${heading}</strong>`);

    // Bold: **text**
    s = s.replace(/\*\*([^*\n]+)\*\*/g, '<strong>$1</strong>');

    // Inline code: `code` (no newlines inside)
    s = s.replace(/`([^`\n]+)`/g, '<code>$1</code>');

    // Newlines → <br>, but preserve newlines inside <pre> blocks
    const parts = s.split(/(<pre[\s\S]*?<\/pre>)/g);
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

    const resp = await fetch('/api/ollama/chat', {
        method: 'POST',
        headers,
        body: JSON.stringify({
            prompt,
            messages: messages.slice(-MAX_HISTORY),
            context,
            lang: i18n.currentLang,
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

    try { // [M1] ensure reader is always released
        while (true) {
            const { value, done } = await reader.read();
            if (done) break;
            buffer += decoder.decode(value, { stream: true });

            const lines = buffer.split('\n');
            buffer = lines.pop(); // keep incomplete last line

            for (const line of lines) {
                if (line.startsWith('event: ')) continue;
                if (!line.startsWith('data: ')) continue;
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

    const inputEl = document.getElementById('ai-input');
    const prompt = (inputEl?.value || '').trim();

    // Clear input
    if (inputEl) inputEl.value = '';

    // Append user message to UI
    if (prompt) {
        appendMessage('user', prompt);
        conversationHistory.push({ role: 'user', content: prompt });
        if (conversationHistory.length > MAX_HISTORY) {
            conversationHistory = conversationHistory.slice(-MAX_HISTORY);
        }
    }

    const context = 'current';
    const assistantBody = appendMessage('assistant', '', true);
    isStreaming = true;
    setUIBusy(true);

    try {
        const fullContent = await streamChatResponse({
            prompt,
            messages: conversationHistory,
            context,
            assistantBody,
        });
        if (fullContent) {
            conversationHistory.push({ role: 'assistant', content: fullContent });
            if (conversationHistory.length > MAX_HISTORY) {
                conversationHistory = conversationHistory.slice(-MAX_HISTORY);
            }
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
        btn.textContent = busy ? '…' : 'Analyse';
    }
}

/**
 * analyzeChartData — opens the AI panel and sends a chart-scoped prompt.
 */
export async function analyzeChartData(chartTitle, csvData) {
    if (isStreaming) return; // [H1]
    openAIPanel();

    const prompt = `Analyse this data for ${chartTitle}.`;

    appendMessage('user', prompt);
    conversationHistory.push({ role: 'user', content: prompt });
    if (conversationHistory.length > MAX_HISTORY) {
        conversationHistory = conversationHistory.slice(-MAX_HISTORY);
    }

    const context = `chart: ${chartTitle}\n${csvData}`;
    const assistantBody = appendMessage('assistant', '', true);
    isStreaming = true;
    setUIBusy(true);

    try {
        const fullContent = await streamChatResponse({
            prompt,
            messages: conversationHistory,
            context,
            assistantBody,
        });
        if (fullContent) {
            conversationHistory.push({ role: 'assistant', content: fullContent });
            if (conversationHistory.length > MAX_HISTORY) {
                conversationHistory = conversationHistory.slice(-MAX_HISTORY);
            }
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
