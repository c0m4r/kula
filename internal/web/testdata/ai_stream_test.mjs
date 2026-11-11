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

const { AI_MAX_ANSWER, AI_REQUEST_LIMITS, appendAnswer, chatRequestBody, createFrameRenderer, createSSEReader } =
    await importSource('../static/js/app/ai-stream.js');

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

test('the answer is kept to AI_MAX_ANSWER, a surrogate pair never split', () => {
    assert.deepEqual(appendAnswer('ab', 'cd', 4), { answer: 'abcd', truncated: false });
    assert.deepEqual(appendAnswer('ab', 'cde', 4), { answer: 'abcd', truncated: true });
    assert.deepEqual(appendAnswer('abc', '😀', 4), { answer: 'abc', truncated: true });
    assert.deepEqual(appendAnswer('abcd', 'e', 4), { answer: 'abcd', truncated: true });

    // A 1 MB answer in 8-byte chunks is cut where the cap falls.
    let answer = '';
    let truncated = false;
    let chunks = 0;
    while (!truncated && chunks < (1 << 20) / 8) {
        ({ answer, truncated } = appendAnswer(answer, 'word **b'));
        chunks++;
    }
    assert.ok(truncated);
    assert.equal(answer.length, AI_MAX_ANSWER);
    assert.equal(chunks, AI_MAX_ANSWER / 8 + 1);
});

// A fake animation-frame clock: frames run when the test says so.
function fakeFrames() {
    let time = 0;
    let nextHandle = 1;
    const callbacks = new Map();
    return {
        now: () => time,
        advance: ms => { time += ms; },
        requestFrame: callback => { callbacks.set(nextHandle, callback); return nextHandle++; },
        cancelFrame: handle => { callbacks.delete(handle); },
        // Run the frames requested so far, as the browser does every ~16 ms.
        frame() {
            time += 16;
            const due = [...callbacks.values()];
            callbacks.clear();
            for (const callback of due) callback(time);
        },
        get pending() { return callbacks.size; },
    };
}

test('repaints are coalesced into animation frames', () => {
    const frames = fakeFrames();
    let renders = 0;
    const view = createFrameRenderer(() => { renders++; }, frames);

    for (let i = 0; i < 1000; i++) view.schedule(); // a burst of chunks
    assert.equal(renders, 0, 'nothing renders before the frame');
    frames.frame();
    assert.equal(renders, 1, 'one render for the whole burst');
    frames.frame();
    assert.equal(renders, 1, 'no render without a new chunk');

    view.schedule();
    view.flush(); // the stream ended: render now
    assert.equal(renders, 2);
    frames.frame();
    assert.equal(renders, 2, 'flush dropped the pending frame');

    view.schedule();
    view.cancel(); // an error replaced the answer
    frames.frame();
    assert.equal(renders, 2, 'cancel dropped the pending frame');
    assert.equal(frames.pending, 0);
});

test('a slow render is followed by a rest three times as long', () => {
    const frames = fakeFrames();
    let renders = 0;
    const view = createFrameRenderer(() => { renders++; frames.advance(20); }, frames);

    view.schedule();
    frames.frame(); // renders, taking 20 ms: no render for the next 60 ms
    assert.equal(renders, 1);
    view.schedule();
    frames.frame(); // +16
    frames.frame(); // +32
    frames.frame(); // +48
    assert.equal(renders, 1, 'rendered again before resting');
    frames.frame(); // +64
    assert.equal(renders, 2);
    view.schedule();
    view.flush(); // the end of the stream does not wait
    assert.equal(renders, 3);
});

test('the request limits are the server\'s', { skip: !process.env.KULA_AI_LIMITS && 'run from ollama_test.go' }, () => {
    assert.deepEqual({ ...AI_REQUEST_LIMITS }, JSON.parse(process.env.KULA_AI_LIMITS));
});

const bytes = text => new TextEncoder().encode(text).length;

test('a short conversation is sent whole, as role and content', () => {
    const history = [
        { role: 'user', content: 'hi' },
        { role: 'assistant', content: 'hello', truncated: false },
        { role: 'user', content: 'and now?' },
    ];
    const body = JSON.parse(chatRequestBody({ prompt: 'and now?', history, context: 'current', lang: 'pl', model: 'm2' }));
    assert.deepEqual(body, {
        prompt: 'and now?',
        messages: history.map(({ role, content }) => ({ role, content })),
        context: 'current',
        lang: 'pl',
        model: 'm2',
    });
    assert.equal('model' in JSON.parse(chatRequestBody({ prompt: 'x', history: [], context: 'current', lang: 'en' })), false);
});

test('each turn and the context are clamped as the server clamps them', () => {
    const { user, assistant, context } = AI_REQUEST_LIMITS;
    let body = JSON.parse(chatRequestBody({
        prompt: '😀'.repeat(user + 5),
        history: [
            { role: 'assistant', content: 'a'.repeat(AI_MAX_ANSWER), truncated: true },
            { role: 'user', content: '😀'.repeat(user + 5) },
        ],
        context: 'current',
        lang: 'en',
    }));
    assert.equal(body.prompt, '😀'.repeat(user)); // characters, not UTF-16 units
    assert.equal(body.messages.length, 2);
    assert.equal(body.messages[0].content, 'a'.repeat(assistant));
    assert.equal(body.messages[1].content, '😀'.repeat(user));
    body = JSON.parse(chatRequestBody({ prompt: 'x', history: [], context: 'c'.repeat(context + 5), lang: 'en' }));
    assert.equal(body.context, 'c'.repeat(context));
});

test('a long conversation drops its oldest turns to fit the server\'s body cap', () => {
    const history = [];
    for (let i = 0; i < 30; i++) {
        history.push({ role: 'user', content: `question ${i} ${'q'.repeat(1500)}` });
        history.push({ role: 'assistant', content: `answer ${i} ${'ä'.repeat(AI_MAX_ANSWER)}`, truncated: true });
    }
    history.push({ role: 'user', content: 'the last question' });
    const text = chatRequestBody({ prompt: 'the last question', history, context: 'x'.repeat(10000), lang: 'en' });
    assert.ok(bytes(text) <= AI_REQUEST_LIMITS.body, `${bytes(text)} bytes`);
    const { messages } = JSON.parse(text);
    assert.ok(messages.length >= 2 && messages.length < AI_REQUEST_LIMITS.messages, `${messages.length} turns`);
    assert.deepEqual(messages.at(-1), { role: 'user', content: 'the last question' });
    assert.deepEqual(messages, history.slice(-messages.length).map(({ role, content }) => ({
        role, content: content.slice(0, role === 'assistant' ? AI_REQUEST_LIMITS.assistant : AI_REQUEST_LIMITS.user),
    })), 'not the newest turns, in order');
});
