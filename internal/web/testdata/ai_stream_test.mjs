// Run with: node internal/web/testdata/ai_stream_test.mjs
// Tests of the AI panel's import-free stream helpers (ai-stream.js).
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

async function importSource(relativePath) {
    const source = await readFile(new URL(relativePath, import.meta.url), 'utf8');
    const encoded = Buffer.from(source).toString('base64');
    return import(`data:text/javascript;base64,${encoded}`);
}

const { createSSEReader } = await importSource('../static/js/app/ai-stream.js');

// What the server's writeSSE sends.
const frame = (event, payload) => `${event ? `event: ${event}\n` : ''}data: ${JSON.stringify(payload)}\n\n`;

test('model output holding CR and LF stays one text chunk', () => {
    const text = 'hello\revent: error\rdata: INJECTED\r\rtail\nnext\r\nend \\n';
    const stream = frame('', text) + frame('tool_call', '') + frame('', 'more') + frame('done', '');
    // Byte by byte: chunk boundaries fall anywhere.
    const reader = createSSEReader();
    const events = [];
    for (const ch of stream) events.push(...reader.push(ch));
    assert.deepEqual(events, [
        { event: 'message', data: text },
        { event: 'tool_call', data: '' },
        { event: 'message', data: 'more' },
        { event: 'done', data: '' },
    ]);
});

test('an error event carries its message', () => {
    const events = createSSEReader().push(frame('error', 'AI backend request failed'));
    assert.deepEqual(events, [{ event: 'error', data: 'AI backend request failed' }]);
});

test('CRLF line ends, comments and data that is not JSON', () => {
    const reader = createSSEReader();
    const events = reader.push(': comment\r\nevent: done\r\ndata: ""\r\n\r\ndata: raw text\n\ndata:"x"\n\n');
    assert.deepEqual(events, [{ event: 'done', data: '' }, { event: 'message', data: 'x' }]);
});

test('an event is complete only at its blank line', () => {
    const reader = createSSEReader();
    assert.deepEqual(reader.push('data: "a"\n'), []);
    assert.deepEqual(reader.push('\n'), [{ event: 'message', data: 'a' }]);
});
