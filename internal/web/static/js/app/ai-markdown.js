/* ============================================================
   ai-markdown.js — renders the AI panel's answers: a markdown
   subset to HTML in which every tag and attribute is the
   renderer's own. Import-free, so the Node tests load it
   directly.
   ============================================================ */
'use strict';

// The answer is untrusted: it can echo hostnames and process names from the
// metrics. Every character of it reaches the output escaped; the only
// attributes built from it are a fenced block's language, when it is a plain
// word, and an ordered list's start number. Nothing is clickable.
//
// The panel repaints the whole answer (up to 32 KiB) on animation frames
// while it streams, so every pass is linear: blocks are parsed line by line,
// and no regular expression can backtrack over more than one line.

const ESCAPES = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
const escapeHTML = (s) => s.replace(/[&<>"']/g, (c) => ESCAPES[c]);

/** Deepest nesting of lists and blockquotes; deeper markers stay text. */
const MAX_DEPTH = 8;

// Inline placeholders: a private-use character, a slot number and another.
// Text is stripped of both characters before it is formatted, so the model
// cannot write one; code keeps them, and is never scanned for placeholders.
const SLOT = /\uE000(\d+)\uE001/g;
const SLOT_CHARS = /[\uE000\uE001]/g;

const FENCE = /^[ \t]*(`{3,}|~{3,})(.*)/;
const FENCE_CLOSE = /^(?:`+|~+)$/;
const LANGUAGE = /^[A-Za-z0-9_+-]{1,32}$/;
const HEADING = /^[ \t]*(#{1,6})[ \t]+(?=\S)/;
const QUOTE = /^[ \t]*>/;
const QUOTE_PREFIX = /^[ \t]*>[ \t]?/;
const LIST_ITEM = /^([ \t]*)(?:[-*+]|(\d{1,9})[.)])[ \t]+/;
const TABLE_ROW = /^[ \t]*\|/;
const ALIGNMENT = /^:?-+:?$/;
const THINK = /^[ \t]*<think>/;
/** The first characters of a line that can start a block other than a paragraph. */
const BLOCK_START = /^[ \t]*[`~*_#|>+\d-]/;

/** Text holding none of these is escaped, and that is all. */
const INLINE_SYNTAX = /[`\\[*_<\uE000\uE001]|:\/\//;
const BACKSLASH = /\\([!-/:-@[-`{-~])/g;
const LINK = /!?\[([^[\]]+)\]\([ \t]*([^\s()]+)[ \t]*\)/g;
const BARE_URL = /\bhttps?:\/\/[^\s<>\uE000\uE001]*[^\s<>\uE000\uE001?!.,:;*_~'"()[\]{}]/gi;
const BREAK_TAG = /<br[ \t]*\/?>/gi;

// Emphasis, strongest first; each match's text is formatted by the passes
// after it. The content of a run never holds its own delimiter pair, so a
// match ends at the next one. Underscores, and single asterisks, only open
// and close at a word boundary: snake_case_name, /var/lib/my_app and
// 2*3*4 stay text, as does 2 * 3 * 4 (an opener is not followed by a space).
// The empty or one-character group before each run is kept as it was.
const EMPHASIS = [
    [/()\*\*(?![\s*])((?:[^*]|\*(?!\*))*?[^\s*])\*\*/g, 'strong'],
    [/(^|[^\p{L}\p{N}])__(?![\s_])((?:[^_]|_(?!_))*?[^\s_])__(?![\p{L}\p{N}])/gu, 'strong'],
    [/(^|[^\p{L}\p{N}*])\*([^\s*](?:[^*]*[^\s*])?)\*(?![\p{L}\p{N}*])/gu, 'em'],
    [/(^|[^\p{L}\p{N}_])_([^\s_](?:[^_]*[^\s_])?)_(?![\p{L}\p{N}_])/gu, 'em'],
];

const ALIGN_CLASSES = { left: ' class="ai-align-left"', center: ' class="ai-align-center"', right: ' class="ai-align-right"' };

/**
 * renderMarkdown returns the HTML of a model's answer: fenced code blocks
 * (verbatim, with a copy button, the closing fence optional while the answer
 * streams), inline code, <think> reasoning as a disclosure (open until its
 * closing tag arrives), headings, rules, bold and italics, nested lists,
 * blockquotes, pipe tables, and links shown as their text and URL. Anything
 * else is escaped text; blank lines separate paragraphs and other line
 * breaks are kept.
 */
export function renderMarkdown(text, { copyLabel = 'Copy', reasoningLabel = 'Reasoning' } = {}) {
    const source = String(text ?? '').replace(/\r\n?/g, '\n');
    const labels = { copy: escapeHTML(String(copyLabel)), reasoning: escapeHTML(String(reasoningLabel)) };
    let html = '';
    for (const part of splitReasoning(source)) {
        html += part.reasoning === undefined
            ? renderBlocks(part.text.split('\n'), 0, false, labels)
            : reasoningBlock(part.reasoning, part.open, labels);
    }
    return html;
}

/**
 * splitReasoning cuts the text into markdown and <think> parts. A <think>
 * tag opens reasoning at the start of a line outside a fenced block; the
 * reasoning runs to </think>, or to the end while it is still streaming.
 */
function splitReasoning(text) {
    const parts = [];
    let from = 0;
    let at = 0;
    let fence = null;
    while (at <= text.length) {
        const newline = text.indexOf('\n', at);
        const end = newline === -1 ? text.length : newline;
        const line = text.slice(at, end);
        if (fence) {
            if (fenceCloses(fence, line)) fence = null;
        } else if (THINK.test(line)) {
            parts.push({ text: text.slice(from, at) });
            const body = at + line.indexOf('<think>') + 7;
            const close = text.indexOf('</think>', body);
            if (close === -1) {
                parts.push({ reasoning: text.slice(body), open: true });
                return parts;
            }
            parts.push({ reasoning: text.slice(body, close), open: false });
            from = close + 8;
            at = from; // what follows the tag starts a line
            continue;
        } else {
            fence = fenceOpen(line);
        }
        if (newline === -1) break;
        at = newline + 1;
    }
    parts.push({ text: text.slice(from) });
    return parts;
}

function reasoningBlock(text, open, labels) {
    const body = text.trim();
    if (!body) return ''; // a model told not to reason sends an empty pair
    return `<details class="ai-think"${open ? ' open' : ''}><summary>${labels.reasoning}</summary>`
        + `<div class="ai-think-body">${escapeHTML(body).replace(/\n/g, '<br>')}</div></details>`;
}

// ---- Blocks ----

const isBlank = (line) => !/\S/.test(line);

/** indentOf returns the columns of leading whitespace, a tab to the next stop of 4. */
function indentOf(line) {
    let col = 0;
    for (let k = 0; k < line.length; k++) {
        if (line[k] === ' ') col++;
        else if (line[k] === '\t') col += 4 - (col % 4);
        else break;
    }
    return col;
}

/** stripIndent removes up to n columns of leading whitespace. */
function stripIndent(line, n) {
    let col = 0;
    let k = 0;
    while (k < line.length && col < n) {
        if (line[k] === ' ') col++;
        else if (line[k] === '\t') col += 4 - (col % 4);
        else break;
        k++;
    }
    return line.slice(k);
}

/**
 * fenceOpen reports an opening code fence, at any indentation: ``` or ~~~
 * (or longer), then an optional info string whose first word, when it is a
 * plain word, names the language. A backtick fence's info has no backtick.
 */
function fenceOpen(line) {
    const m = FENCE.exec(line);
    if (!m || (m[1][0] === '`' && m[2].includes('`'))) return null;
    const word = m[2].trim().split(/[ \t]/, 1)[0];
    return { marker: m[1], indent: indentOf(line), language: LANGUAGE.test(word) ? word : '' };
}

/** fenceCloses reports a line closing the fence: its character, at least as many, nothing else. */
function fenceCloses(fence, line) {
    const t = line.trim();
    return t.length >= fence.marker.length && t[0] === fence.marker[0] && FENCE_CLOSE.test(t);
}

/** ruleKind returns the character of a horizontal rule (---, ***, ___, spaces allowed), or ''. */
function ruleKind(line) {
    let kind = '';
    let count = 0;
    for (let k = 0; k < line.length; k++) {
        const c = line[k];
        if (c === ' ' || c === '\t') continue;
        if (kind ? c !== kind : c !== '-' && c !== '*' && c !== '_') return '';
        kind = c;
        count++;
    }
    return count >= 3 ? kind : '';
}

/** listItem parses a list item's first line, or returns null. */
function listItem(line) {
    const m = LIST_ITEM.exec(line);
    if (!m || ruleKind(line)) return null;
    return {
        ordered: m[2] !== undefined,
        number: m[2] === undefined ? 1 : Number(m[2]),
        indent: indentOf(m[1]),
        content: indentOf(m[0].replace(/[-*+.)\d]/g, ' ')), // the column the item's text starts at
        text: line.slice(m[0].length),
    };
}

/** splitCells splits a table row at the pipes a backslash does not escape. */
function splitCells(row) {
    let s = row.trim();
    if (s.startsWith('|')) s = s.slice(1);
    if (s.endsWith('|') && !s.endsWith('\\|')) s = s.slice(0, -1);
    const cells = [];
    let start = 0;
    for (let k = 0; k < s.length; k++) {
        if (s[k] === '\\') k++;
        else if (s[k] === '|') {
            cells.push(s.slice(start, k).trim());
            start = k + 1;
        }
    }
    cells.push(s.slice(start).trim());
    return cells;
}

/** isTableStart reports a header row starting with | over a separator row such as |:--|--:|. */
function isTableStart(lines, i) {
    if (i + 1 >= lines.length || !TABLE_ROW.test(lines[i])) return false;
    const separator = lines[i + 1].trim();
    return separator.startsWith('|') && splitCells(separator).every((cell) => ALIGNMENT.test(cell));
}

function startsBlock(lines, i, depth) {
    const line = lines[i];
    if (!BLOCK_START.test(line)) return false;
    return Boolean(fenceOpen(line) || ruleKind(line) || HEADING.test(line) || isTableStart(lines, i)
        || (depth < MAX_DEPTH && (QUOTE.test(line) || listItem(line))));
}

/**
 * renderBlocks renders lines as blocks. tight renders paragraphs without
 * <p>, as the items of a list with no blank line between them are.
 */
function renderBlocks(lines, depth, tight, labels) {
    let html = '';
    let i = 0;
    while (i < lines.length) {
        const line = lines[i];
        if (isBlank(line)) {
            i++;
            continue;
        }

        const fence = fenceOpen(line);
        if (fence) {
            const code = [];
            for (i++; i < lines.length && !fenceCloses(fence, lines[i]); i++) {
                code.push(stripIndent(lines[i], fence.indent));
            }
            i++; // the closing fence, once it has arrived
            html += codeBlock(code.join('\n'), fence.language, labels);
            continue;
        }

        if (ruleKind(line)) {
            html += '<hr>';
            i++;
            continue;
        }

        const heading = HEADING.exec(line);
        if (heading) {
            const level = heading[1].length;
            html += `<div class="ai-h ai-h${level}" role="heading" aria-level="${Math.min(level + 2, 6)}">`
                + `${renderInline(withoutClosingHashes(line.slice(heading[0].length)))}</div>`;
            i++;
            continue;
        }

        if (depth < MAX_DEPTH && QUOTE.test(line)) {
            const quoted = [];
            for (; i < lines.length; i++) {
                if (QUOTE.test(lines[i])) {
                    quoted.push(lines[i].replace(QUOTE_PREFIX, ''));
                } else if (!isBlank(quoted[quoted.length - 1]) && !isBlank(lines[i]) && !startsBlock(lines, i, depth)) {
                    quoted.push(lines[i]); // a paragraph's lazy continuation
                } else {
                    break;
                }
            }
            html += `<blockquote>${renderBlocks(quoted, depth + 1, false, labels)}</blockquote>`;
            continue;
        }

        if (isTableStart(lines, i)) {
            const header = splitCells(line);
            const align = splitCells(lines[i + 1]).map((cell) => {
                if (cell.startsWith(':')) return cell.length > 1 && cell.endsWith(':') ? 'center' : 'left';
                return cell.endsWith(':') ? 'right' : '';
            });
            const rows = [];
            for (i += 2; i < lines.length && TABLE_ROW.test(lines[i]); i++) rows.push(splitCells(lines[i]));
            html += renderTable(header, align, rows);
            continue;
        }

        if (depth < MAX_DEPTH && listItem(line)) {
            const list = renderList(lines, i, depth, labels);
            html += list.html;
            i = list.next;
            continue;
        }

        // A paragraph, to a blank line or the next block. A line of dashes
        // under its text is text: there are no underlined headings.
        const paragraph = [line.trim()];
        for (i++; i < lines.length && !isBlank(lines[i]); i++) {
            if (ruleKind(lines[i]) !== '-' && startsBlock(lines, i, depth)) break;
            paragraph.push(lines[i].trim());
        }
        const body = paragraph.map(renderInline).join('<br>');
        html += tight ? body : `<p>${body}</p>`;
    }
    return html;
}

function codeBlock(code, language, labels) {
    const cls = language ? ` class="language-${language}"` : '';
    return `<div class="ai-code"><button type="button" class="ai-code-copy">${labels.copy}</button>`
        + `<pre dir="ltr"><code${cls}>${escapeHTML(code)}</code></pre></div>`;
}

/** withoutClosingHashes drops a heading's optional closing run of #. */
function withoutClosingHashes(text) {
    const t = text.trimEnd();
    let end = t.length;
    while (end > 0 && t[end - 1] === '#') end--;
    if (end === t.length || (end > 0 && t[end - 1] !== ' ' && t[end - 1] !== '\t')) return t;
    return t.slice(0, end).trimEnd();
}

function renderTable(header, align, rows) {
    const cells = (row, tag) => row.map((cell, k) => `<${tag}${ALIGN_CLASSES[align[k]] ?? ''}>${renderInline(cell)}</${tag}>`).join('');
    let html = `<table class="ai-table"><thead><tr>${cells(header, 'th')}</tr></thead>`;
    if (rows.length) html += `<tbody>${rows.map((row) => `<tr>${cells(row, 'td')}</tr>`).join('')}</tbody>`;
    return `${html}</table>`;
}

/**
 * renderList renders the list starting at lines[start] and returns its HTML
 * and the index of the first line after it. A line belongs to an item when
 * it is indented by at least two columns more than the item's marker (or up
 * to the item's text), when it continues the item's paragraph, or while a
 * fence opened in the item is still open. Items of the same kind follow each
 * other; a blank line between them, or between an item's blocks, makes the
 * list loose (its paragraphs get <p>).
 */
function renderList(lines, start, depth, labels) {
    const first = listItem(lines[start]);
    const items = [];
    let loose = false;
    let i = start;
    let item = first;
    for (;;) {
        const threshold = Math.min(item.content, item.indent + 2);
        const body = [item.text];
        let fence = fenceOpen(item.text);
        let blank = false;
        for (i++; i < lines.length; i++) {
            const line = lines[i];
            if (fence) {
                body.push(stripIndent(line, item.content));
                if (fenceCloses(fence, line)) fence = null;
            } else if (isBlank(line)) {
                body.push('');
                blank = true;
            } else if (indentOf(line) >= threshold) {
                const text = stripIndent(line, item.content);
                if (blank) loose = true;
                body.push(text);
                fence = fenceOpen(text);
                blank = false;
            } else if (!blank && !startsBlock(lines, i, depth)) {
                body.push(line.trim()); // a paragraph's lazy continuation
            } else {
                break;
            }
        }
        while (body.length > 1 && isBlank(body[body.length - 1])) body.pop();
        items.push(body);
        const next = i < lines.length ? listItem(lines[i]) : null;
        if (!next || next.ordered !== first.ordered) break;
        if (blank) loose = true;
        item = next;
    }
    const tag = first.ordered ? 'ol' : 'ul';
    const startAttr = first.ordered && first.number !== 1 ? ` start="${first.number}"` : '';
    const html = items.map((body) => `<li>${renderBlocks(body, depth + 1, !loose, labels)}</li>`).join('');
    return { html: `<${tag}${startAttr}>${html}</${tag}>`, next: i };
}

// ---- Inline ----

/**
 * renderInline renders one line of text: code spans first, verbatim, then
 * backslash escapes, links, bare URLs and <br> tags, each held in a
 * placeholder while the escaped rest gets its emphasis.
 */
function renderInline(line) {
    if (!INLINE_SYNTAX.test(line)) return escapeHTML(line);
    const slots = [];
    const hold = (html) => `\uE000${slots.push(html) - 1}\uE001`;
    const resolve = (s) => s.replace(SLOT, (_, n) => slots[n]);
    let s = holdCodeSpans(line, hold);
    s = s.replace(BACKSLASH, (_, c) => hold(escapeHTML(c)));
    s = s.replace(LINK, (_, label, url) => {
        const address = resolve(escapeHTML(url));
        if (label.trim() === url) return hold(`<span class="ai-link-url" dir="ltr">${address}</span>`);
        return hold(`${resolve(formatText(label, hold, resolve))} <span class="ai-link-url" dir="ltr">(${address})</span>`);
    });
    return resolve(formatText(s, hold, resolve));
}

function formatText(s, hold, resolve) {
    const held = s.replace(BARE_URL, (url) => hold(escapeHTML(url))).replace(BREAK_TAG, () => hold('<br>'));
    return emphasize(escapeHTML(held), 0, hold, resolve);
}

function emphasize(s, from, hold, resolve) {
    for (let p = from; p < EMPHASIS.length; p++) {
        const [pattern, tag] = EMPHASIS[p];
        s = s.replace(pattern, (_, before, content) =>
            before + hold(`<${tag}>${resolve(emphasize(content, p + 1, hold, resolve))}</${tag}>`));
    }
    return s;
}

/**
 * holdCodeSpans replaces each code span with a placeholder and strips the
 * placeholder characters from the rest. A run of backticks opens a span
 * that the next run of exactly as many closes; a run with no such partner,
 * or whose first backtick a backslash escapes, is text.
 */
function holdCodeSpans(line, hold) {
    const runs = [];
    for (let k = 0; k < line.length; k++) {
        if (line[k] !== '`') continue;
        let end = k + 1;
        while (end < line.length && line[end] === '`') end++;
        runs.push([k, end - k]);
        k = end - 1;
    }
    if (!runs.length) return line.replace(SLOT_CHARS, '');
    // For each run length, the indices of the runs that long and how far a
    // search has moved through them: the scan only moves forward.
    const byLength = new Map();
    runs.forEach(([, length], r) => {
        const entry = byLength.get(length);
        if (entry) entry.runs.push(r);
        else byLength.set(length, { runs: [r], next: 0 });
    });
    const closer = (length, after) => {
        const entry = byLength.get(length);
        if (!entry) return -1;
        while (entry.next < entry.runs.length && entry.runs[entry.next] <= after) entry.next++;
        return entry.next < entry.runs.length ? entry.runs[entry.next] : -1;
    };
    let out = '';
    let pos = 0;
    for (let r = 0; r < runs.length; r++) {
        let [at, length] = runs[r];
        let slashes = 0;
        while (at - slashes > pos && line[at - slashes - 1] === '\\') slashes++;
        if (slashes % 2) {
            at++;
            length--;
            if (!length) continue;
        }
        const c = closer(length, r);
        if (c === -1) continue;
        const [end, closeLength] = runs[c];
        let code = line.slice(at + length, end);
        if (code.length > 1 && code[0] === ' ' && code[code.length - 1] === ' ' && /[^ ]/.test(code)) code = code.slice(1, -1);
        out += line.slice(pos, at).replace(SLOT_CHARS, '') + hold(`<code dir="ltr">${escapeHTML(code)}</code>`);
        pos = end + closeLength;
        r = c;
    }
    return out + line.slice(pos).replace(SLOT_CHARS, '');
}
