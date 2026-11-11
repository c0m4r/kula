/* ============================================================
   ai-stream.js — reading the AI chat's Server-Sent Events.
   Import-free, so the Node tests load it directly.
   ============================================================ */
'use strict';

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
            buffer += text;
            const lines = buffer.split('\n');
            buffer = lines.pop(); // keep the incomplete last line
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
