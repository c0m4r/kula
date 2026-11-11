/* ============================================================
   ai-stream.js — the AI chat's request and Server-Sent Events,
   and bounding what showing the answer costs. Import-free, so
   the Node tests load it directly.
   ============================================================ */
'use strict';

/**
 * Longest answer, in UTF-16 code units, the AI panel keeps and shows: far
 * above an ordinary answer, reasoning included. Past it the answer is cut
 * and the stream closed. Painting an answer this long, dense with markdown,
 * takes up to ~150 ms on a desktop.
 */
export const AI_MAX_ANSWER = 32 * 1024;

/**
 * appendAnswer adds a streamed chunk to the answer so far, keeping at most
 * max code units (a surrogate pair is never split). truncated reports that
 * something was left out.
 */
export function appendAnswer(answer, chunk, max = AI_MAX_ANSWER) {
    if (answer.length + chunk.length <= max) return { answer: answer + chunk, truncated: false };
    let room = Math.max(0, max - answer.length);
    const last = chunk.charCodeAt(room - 1);
    if (room > 0 && last >= 0xd800 && last <= 0xdbff) room--;
    return { answer: answer + chunk.slice(0, room), truncated: true };
}

/**
 * createFrameRenderer coalesces repaints: schedule() asks for render() on an
 * animation frame, once however many chunks arrive before it; flush()
 * renders now, dropping a pending frame; cancel() drops it without rendering.
 * After a render it waits restFactor times as long as that render took
 * before the next, so a long answer's repaints take at most a quarter of
 * the main thread. Re-rendering the whole answer on every chunk made a long
 * answer in small chunks cost minutes of main-thread time.
 */
export function createFrameRenderer(render, {
    requestFrame = callback => globalThis.requestAnimationFrame(callback),
    cancelFrame = handle => globalThis.cancelAnimationFrame(handle),
    now = () => globalThis.performance.now(),
    restFactor = 3,
} = {}) {
    let pending = null;
    let notBefore = -Infinity;
    const paint = () => {
        const start = now();
        render();
        const end = now();
        notBefore = end + (end - start) * restFactor;
    };
    const frame = () => {
        if (now() < notBefore) {
            pending = requestFrame(frame);
            return;
        }
        pending = null;
        paint();
    };
    const cancel = () => {
        if (pending === null) return;
        cancelFrame(pending);
        pending = null;
    };
    return {
        schedule() {
            if (pending === null) pending = requestFrame(frame);
        },
        flush() {
            cancel();
            paint();
        },
        cancel,
    };
}

/**
 * createSSEReader returns a parser for the /api/ollama/chat stream. push()
 * takes decoded text and returns the events it completed, each
 * { event, data }: event is 'message' unless an "event:" field names another,
 * and data is the JSON-decoded payload. The server encodes every payload as a
 * JSON string, so text holding a CR or LF cannot end a line early; an event
 * whose data is not JSON is skipped.
 */
export function createSSEReader() {
    let buffer = '';
    let event = '';
    let data = null;
    return {
        push(text) {
            const end = text.lastIndexOf('\n');
            if (end === -1) {
                buffer += text; // still inside one line: nothing to scan again
                return [];
            }
            const lines = (buffer + text.slice(0, end)).split('\n');
            buffer = text.slice(end + 1); // keep the incomplete last line
            const events = [];
            for (let line of lines) {
                if (line.endsWith('\r')) line = line.slice(0, -1);
                if (line === '') {
                    if (data !== null) {
                        try {
                            events.push({ event: event || 'message', data: JSON.parse(data) });
                        } catch {
                            // Not a payload the server writes.
                        }
                    }
                    event = '';
                    data = null;
                } else if (line.startsWith('event:')) {
                    event = line.slice(6).trim();
                } else if (line.startsWith('data:')) {
                    const value = line.slice(line.startsWith('data: ') ? 6 : 5);
                    data = data === null ? value : `${data}\n${value}`;
                }
            }
            return events;
        },
    };
}
