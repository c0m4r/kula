// Run with: node internal/web/testdata/ai_markdown_test.mjs
// Tests of the AI panel's import-free markdown renderer (ai-markdown.js). When
// KULA_AI_MARKDOWN_MINIFIED names a file (TestAIMarkdownFrontend passes the
// module as the server serves it), every test also runs on that file.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { performance } from 'node:perf_hooks';
import test from 'node:test';

async function importFile(url) {
    const source = await readFile(url, 'utf8');
    const encoded = Buffer.from(source).toString('base64');
    return import(`data:text/javascript;base64,${encoded}`);
}

const variants = [['source', (await importFile(new URL('../static/js/app/ai-markdown.js', import.meta.url))).renderMarkdown]];
if (process.env.KULA_AI_MARKDOWN_MINIFIED) {
    variants.push(['minified', (await importFile(process.env.KULA_AI_MARKDOWN_MINIFIED)).renderMarkdown]);
}

// ---- What the renderer may write ----

const TAGS = new Set([
    'blockquote', 'br', 'button', 'code', 'details', 'div', 'em', 'hr', 'li', 'ol', 'p', 'pre', 'span',
    'strong', 'summary', 'table', 'tbody', 'td', 'th', 'thead', 'tr', 'ul',
]);
const VOID = new Set(['br', 'hr']);
const CLASSES = new Set([
    'ai-align-center', 'ai-align-left', 'ai-align-right', 'ai-code', 'ai-code-copy', 'ai-h', 'ai-h1', 'ai-h2',
    'ai-h3', 'ai-h4', 'ai-h5', 'ai-h6', 'ai-link-url', 'ai-table', 'ai-think', 'ai-think-body',
]);
const ATTRIBUTES = {
    class: (v) => v !== undefined && v.split(' ').every((c) => CLASSES.has(c) || /^language-[A-Za-z0-9_+-]{1,32}$/.test(c)),
    dir: (v) => v === 'ltr',
    role: (v) => v === 'heading',
    'aria-level': (v) => /^[3-6]$/.test(v),
    start: (v) => /^\d{1,9}$/.test(v),
    type: (v) => v === 'button',
    open: (v) => v === undefined,
};
const TAG = /<(\/?)([a-z]+)((?: [a-z-]+(?:="[^"]*")?)*)>/y;
const ATTRIBUTE = / ([a-z-]+)(?:="([^"]*)")?/g;

/**
 * assertMarkup fails unless html is text with nothing unescaped, in
 * well-nested elements and attributes the renderer writes, and with no
 * placeholder character left outside code and reasoning (which keep the
 * model's text as it is).
 */
function assertMarkup(html) {
    const open = [];
    let at = 0;
    while (at < html.length) {
        const lt = html.indexOf('<', at);
        const text = html.slice(at, lt === -1 ? html.length : lt);
        assert.doesNotMatch(text, /[>"']|&(?!(?:amp|lt|gt|quot|#39);)/, `text not escaped: ${JSON.stringify(text.slice(0, 80))}`);
        if (!open.includes('code') && !open.includes('details')) {
            assert.doesNotMatch(text, /[\uE000\uE001]/, `a placeholder is left in ${JSON.stringify(text.slice(0, 80))}`);
        }
        if (lt === -1) break;
        TAG.lastIndex = lt;
        const m = TAG.exec(html);
        assert.ok(m, `markup the renderer does not write: ${JSON.stringify(html.slice(lt, lt + 80))}`);
        const [, closing, name, attributes] = m;
        assert.ok(TAGS.has(name), `element <${name}>`);
        if (closing) {
            assert.equal(attributes, '');
            assert.equal(open.pop(), name, `</${name}> closes another element`);
        } else {
            for (const [, attribute, value] of attributes.matchAll(ATTRIBUTE)) {
                assert.ok(Object.hasOwn(ATTRIBUTES, attribute) && ATTRIBUTES[attribute](value), `${attribute}=${JSON.stringify(value)} on <${name}>`);
            }
            if (!VOID.has(name)) open.push(name);
        }
        at = TAG.lastIndex;
    }
    assert.deepEqual(open, [], 'elements left open');
}

/** check defines a test of each module variant; render checks its every output with assertMarkup. */
function check(name, fn) {
    for (const [variant, renderMarkdown] of variants) {
        const render = (text, options) => {
            const html = renderMarkdown(text, options);
            assertMarkup(html);
            return html;
        };
        test(variants.length > 1 ? `${name} [${variant}]` : name, () => fn(render, renderMarkdown));
    }
}

const ENTITIES = { amp: '&', lt: '<', gt: '>', quot: '"', '#39': "'" };
const decode = (s) => s.replace(/&(amp|lt|gt|quot|#39);/g, (_, e) => ENTITIES[e]);

/** codeBlocks returns each fenced block's language and its text as the copy button copies it. */
const codeBlocks = (html) => [...html.matchAll(/<pre dir="ltr"><code(?: class="language-([^"]+)")?>([^<]*)<\/code><\/pre>/g)]
    .map(([, language = '', text]) => ({ language, text: decode(text) }));

const fenced = (language, code) => `\`\`\`${language}\n${code}\n\`\`\``;

// ---- Code ----

check('a fenced block keeps comments, rules, bold and backticks verbatim (M1)', (render) => {
    const code = [
        'vmstat 1 5',
        '# check memory pressure',
        'free -h',
        '---',
        'echo "**not bold**"',
        'echo `date`',
        '  ## indented # comment',
    ].join('\n');
    const html = render(`Check memory:\n\n${fenced('bash', code)}\n\nThen compare.`);
    assert.deepEqual(codeBlocks(html), [{ language: 'bash', text: code }]);
    assert.ok(html.startsWith('<p>Check memory:</p><div class="ai-code"><button type="button" class="ai-code-copy">Copy</button><pre dir="ltr"><code class="language-bash">'));
    assert.ok(html.endsWith('</code></pre></div><p>Then compare.</p>'));
    assert.doesNotMatch(html, /<hr>|<strong>|<em>|role="heading"/);
});

check('a fenced YAML block keeps its document separators', (render) => {
    const code = '---\napiVersion: v1\nkind: Pod\nmetadata:\n  name: web_app_1\n  labels: {app: "*web*"}\n---\nkind: Service\n...';
    const html = render(`Two documents:\n${fenced('yaml', code)}`);
    assert.deepEqual(codeBlocks(html), [{ language: 'yaml', text: code }]);
    assert.doesNotMatch(html, /<hr>|<em>/);
});

check('an unclosed fence renders the code so far while the answer streams', (render) => {
    assert.deepEqual(codeBlocks(render('Try:\n```sh\nuptime\n# load')), [{ language: 'sh', text: 'uptime\n# load' }]);
    assert.deepEqual(codeBlocks(render('Try:\n```sh')), [{ language: 'sh', text: '' }]);
    // Everything after an opening fence is code until it closes.
    const html = render('```\n**a**\n\n# b\n- c\n| d |\n|---|');
    assert.deepEqual(codeBlocks(html), [{ language: '', text: '**a**\n\n# b\n- c\n| d |\n|---|' }]);
    assert.doesNotMatch(html, /<strong>|<ul>|<table|role="heading"/);
});

check('fences: tildes, longer fences, info strings and blocks in a row', (render) => {
    assert.deepEqual(codeBlocks(render('~~~python\nprint("```")\n~~~')), [{ language: 'python', text: 'print("```")' }]);
    assert.deepEqual(codeBlocks(render('````md\n```bash\nls\n```\n````')), [{ language: 'md', text: '```bash\nls\n```' }]);
    assert.deepEqual(codeBlocks(render('```bash title="x" {1}\nls\n```')), [{ language: 'bash', text: 'ls' }]);
    // A closing fence takes no info string, and no shorter run closes.
    assert.deepEqual(codeBlocks(render('```\na\n```js\n``\n```')), [{ language: '', text: 'a\n```js\n``' }]);
    assert.deepEqual(codeBlocks(render(`${fenced('a', '1')}\n${fenced('b', '2')}`)), [{ language: 'a', text: '1' }, { language: 'b', text: '2' }]);
    // An indented fence drops its indentation from the code, no more.
    assert.deepEqual(codeBlocks(render('  ```\n  a\n    b\nc\n  ```')), [{ language: '', text: 'a\n  b\nc' }]);
    // Tabs and trailing spaces are kept.
    assert.deepEqual(codeBlocks(render('```make\nall:\n\tgo build  \n```')), [{ language: 'make', text: 'all:\n\tgo build  ' }]);
    // Three backticks with a backtick after them are inline code, not a fence.
    assert.equal(render('``` echo `x` ```'), '<p><code dir="ltr">echo `x`</code></p>');
    assert.equal(render('```a````'), '<p>```a````</p>');
});

check('a code block labels its copy button', (render) => {
    const html = render(fenced('', 'x'), { copyLabel: 'Kopieren' });
    assert.equal(html, '<div class="ai-code"><button type="button" class="ai-code-copy">Kopieren</button><pre dir="ltr"><code>x</code></pre></div>');
    assert.match(render(fenced('', 'x'), { copyLabel: '<b>"Copy"</b>' }), /class="ai-code-copy">&lt;b&gt;&quot;Copy&quot;&lt;\/b&gt;<\/button>/);
});

check('inline code is verbatim, ** and backticks included', (render) => {
    assert.equal(
        render('Use `**kwargs`, `a*b*c`, `snake_case` and `[x](y)`.'),
        '<p>Use <code dir="ltr">**kwargs</code>, <code dir="ltr">a*b*c</code>, <code dir="ltr">snake_case</code> and <code dir="ltr">[x](y)</code>.</p>',
    );
    assert.equal(render('Run `` echo `date` `` now'), '<p>Run <code dir="ltr">echo `date`</code> now</p>');
    assert.equal(render('`a`b`'), '<p><code dir="ltr">a</code>b`</p>');
    assert.equal(render('an `unclosed span'), '<p>an `unclosed span</p>');
    assert.equal(render('\\`not code\\`'), '<p>`not code`</p>');
    assert.equal(render('**bold `code**` x**'), '<p><strong>bold <code dir="ltr">code**</code> x</strong></p>');
    assert.equal(render('`<b>&amp;</b>`'), '<p><code dir="ltr">&lt;b&gt;&amp;amp;&lt;/b&gt;</code></p>');
});

// ---- Blocks ----

check('paragraphs and line breaks', (render) => {
    assert.equal(render('a\nb\n\n\nc'), '<p>a<br>b</p><p>c</p>');
    assert.equal(render('  indented\n\ttext  '), '<p>indented<br>text</p>');
    assert.equal(render(''), '');
    assert.equal(render('\n \n'), '');
    assert.equal(render('a\r\nb\rc'), '<p>a<br>b<br>c</p>');
    assert.equal(render(null), '');
});

check('horizontal rules, and dashes under text that stay text', (render) => {
    assert.equal(render('a\n\n---\n\nb'), '<p>a</p><hr><p>b</p>');
    assert.equal(render('***\n___\n- - -\n* * *'), '<hr><hr><hr><hr>');
    assert.equal(render('Title\n---\nmore'), '<p>Title<br>---<br>more</p>');
    assert.equal(render('a\n***'), '<p>a</p><hr>');
    assert.equal(render('--\n-'), '<p>--<br>-</p>');
});

check('headings of every level sit in the bubble as aria headings', (render) => {
    const html = render('# One\n## Two\n### Three\n#### Four\n##### Five\n###### Six');
    const levels = [...html.matchAll(/<div class="ai-h ai-h(\d)" role="heading" aria-level="(\d)">(\w+)<\/div>/g)].map((m) => m.slice(1));
    assert.deepEqual(levels, [['1', '3', 'One'], ['2', '4', 'Two'], ['3', '5', 'Three'], ['4', '6', 'Four'], ['5', '6', 'Five'], ['6', '6', 'Six']]);
    assert.doesNotMatch(html, /<h\d/);
    assert.equal(render('## Disk ##'), '<div class="ai-h ai-h2" role="heading" aria-level="4">Disk</div>');
    assert.equal(render('### **CPU** `load` #1'), '<div class="ai-h ai-h3" role="heading" aria-level="5"><strong>CPU</strong> <code dir="ltr">load</code> #1</div>');
    assert.equal(render('####### seven\n#hashtag\n#'), '<p>####### seven<br>#hashtag<br>#</p>');
    assert.equal(render('Intro\n## Next\ntext'), '<p>Intro</p><div class="ai-h ai-h2" role="heading" aria-level="4">Next</div><p>text</p>');
});

check('lists: nested by indentation, ordered with a start number', (render) => {
    assert.equal(
        render('- one\n  - nested *a*\n    - deeper\n- two'),
        '<ul><li>one<ul><li>nested <em>a</em><ul><li>deeper</li></ul></li></ul></li><li>two</li></ul>',
    );
    assert.equal(render('* a\n+ b\n- c'), '<ul><li>a</li><li>b</li><li>c</li></ul>');
    assert.equal(render('3. three\n4. **four**'), '<ol start="3"><li>three</li><li><strong>four</strong></li></ol>');
    assert.equal(render('1) a\n2) b'), '<ol><li>a</li><li>b</li></ol>');
    assert.equal(render('0. zero'), '<ol start="0"><li>zero</li></ol>');
    // Two spaces nest under an ordered item, whose text starts at three.
    assert.equal(
        render('1. CPU\n  - user\n   - system\n2. Memory\n    1. used\n    2. cached'),
        '<ol><li>CPU<ul><li>user</li><li>system</li></ul></li><li>Memory<ol><li>used</li><li>cached</li></ol></li></ol>',
    );
    // A paragraph is followed by a list without a blank line between them.
    assert.equal(render('Steps:\n- a\n- b\nafter b'), '<p>Steps:</p><ul><li>a</li><li>b<br>after b</li></ul>');
    assert.equal(render('- a\n\ntext'), '<ul><li>a</li></ul><p>text</p>');
    // Ordered and unordered items make separate lists.
    assert.equal(render('- a\n1. b'), '<ul><li>a</li></ul><ol><li>b</li></ol>');
    // Not lists.
    assert.equal(render('-1 and +5% and 1.5 GB and **bold**'), '<p>-1 and +5% and 1.5 GB and <strong>bold</strong></p>');
});

check('loose lists put their paragraphs in <p>', (render) => {
    assert.equal(render('1. a\n\n2. b'), '<ol><li><p>a</p></li><li><p>b</p></li></ol>');
    assert.equal(render('- a\n\n  more\n- b'), '<ul><li><p>a</p><p>more</p></li><li><p>b</p></li></ul>');
    // Blank lines in an item's code block do not make the list loose.
    const html = render('- a\n  ```\n  x\n\n  y\n  ```\n- b');
    assert.equal(html, '<ul><li>a<div class="ai-code"><button type="button" class="ai-code-copy">Copy</button><pre dir="ltr"><code>x\n\ny</code></pre></div></li><li>b</li></ul>');
});

check('code blocks inside list items', (render) => {
    const html = render('1. Run:\n   ```bash\n   free -h\n   # x\n   ```\n2. Next');
    assert.deepEqual(codeBlocks(html), [{ language: 'bash', text: 'free -h\n# x' }]);
    assert.match(html, /^<ol><li>Run:<div class="ai-code">.*<\/div><\/li><li>Next<\/li><\/ol>$/s);
    // A fence opened in an item holds its lines however little they are indented.
    assert.deepEqual(codeBlocks(render('1. Run:\n   ```bash\n# comment\n- not a list\n   ```\n2. Next')), [{ language: 'bash', text: '# comment\n- not a list' }]);
    // A fence at the margin ends the list; the next item keeps its number.
    const split = render('1. a\n```\nx\n```\n2. b');
    assert.match(split, /^<ol><li>a<\/li><\/ol><div class="ai-code">.*<\/div><ol start="2"><li>b<\/li><\/ol>$/s);
    // An item can start with a fence.
    assert.deepEqual(codeBlocks(render('- ```sh\n  ls -l\n  ```')), [{ language: 'sh', text: 'ls -l' }]);
});

check('blockquotes', (render) => {
    assert.equal(render('> **Note**: x\n> y'), '<blockquote><p><strong>Note</strong>: x<br>y</p></blockquote>');
    assert.equal(render('> a\nlazy\n\nout'), '<blockquote><p>a<br>lazy</p></blockquote><p>out</p>');
    assert.equal(render('> a\n> > b'), '<blockquote><p>a</p><blockquote><p>b</p></blockquote></blockquote>');
    assert.equal(render('> - a\n> - b'), '<blockquote><ul><li>a</li><li>b</li></ul></blockquote>');
    assert.equal(render('a > b'), '<p>a &gt; b</p>');
});

check('tables keep inline formatting and alignment', (render) => {
    const html = render([
        '| Metric | **Value** | Note | Plain |',
        '|:-------|------:|:----:|---|',
        '| `cpu_usage` | *42* % | a \\| b<br>c | [x](http://h/y) |',
        '| mem | 1 |',
    ].join('\n'));
    assert.equal(html, [
        '<table class="ai-table"><thead><tr>',
        '<th class="ai-align-left">Metric</th><th class="ai-align-right"><strong>Value</strong></th>',
        '<th class="ai-align-center">Note</th><th>Plain</th>',
        '</tr></thead><tbody>',
        '<tr><td class="ai-align-left"><code dir="ltr">cpu_usage</code></td><td class="ai-align-right"><em>42</em> %</td>',
        '<td class="ai-align-center">a | b<br>c</td><td>x <span class="ai-link-url" dir="ltr">(http://h/y)</span></td></tr>',
        '<tr><td class="ai-align-left">mem</td><td class="ai-align-right">1</td></tr>',
        '</tbody></table>',
    ].join(''));
    // A header without its separator row yet is text; a row without its closing pipe is a row.
    assert.equal(render('| a | b |'), '<p>| a | b |</p>');
    assert.equal(render('| a | b |\n|---|---|\n| 1 | 2'), '<table class="ai-table"><thead><tr><th>a</th><th>b</th></tr></thead><tbody><tr><td>1</td><td>2</td></tr></tbody></table>');
    assert.equal(render('Text\n| a |\n|---|\n\nafter'), '<p>Text</p><table class="ai-table"><thead><tr><th>a</th></tr></thead></table><p>after</p>');
});

// ---- Inline ----

check('bold and italics, and the underscores and asterisks that are not', (render) => {
    const cases = [
        ['*italic* and _also_', '<em>italic</em> and <em>also</em>'],
        ['**bold** and __bold__', '<strong>bold</strong> and <strong>bold</strong>'],
        ['***both*** and ___both___', '<em><strong>both</strong></em> and <em><strong>both</strong></em>'],
        ['**bold with *italic* inside**', '<strong>bold with <em>italic</em> inside</strong>'],
        ['*italic with **bold** inside*', '<em>italic with <strong>bold</strong> inside</em>'],
        ['**a * b**', '<strong>a * b</strong>'],
        ['__my_var__ is set', '<strong>my_var</strong> is set'],
        ['(*a*), "_b_", **c**.', '(<em>a</em>), &quot;<em>b</em>&quot;, <strong>c</strong>.'],
        ['snake_case_name and __dunder_like', 'snake_case_name and __dunder_like'],
        ['/var/lib/my_app/data_dir and C:\\Program_Files_x86', '/var/lib/my_app/data_dir and C:\\Program_Files_x86'],
        ['vm.dirty_ratio_x and файл_имя_тест', 'vm.dirty_ratio_x and файл_имя_тест'],
        ['2 * 3 * 4 = 24, 2*3*4 = 24', '2 * 3 * 4 = 24, 2*3*4 = 24'],
        ['a * b and c _ d', 'a * b and c _ d'],
        ['**unclosed bold and *unclosed italic', '**unclosed bold and *unclosed italic'],
        ['** not bold** and * not italic*', '** not bold** and * not italic*'],
        ['**not bold ** either', '**not bold ** either'],
        ['\\*not italic\\* and \\_not\\_', '*not italic* and _not_'],
        ['*.log and *.txt', '*.log and *.txt'],
    ];
    for (const [text, html] of cases) assert.equal(render(text), `<p>${html}</p>`, text);
    // Emphasis stays on its line.
    assert.equal(render('**a\nb**'), '<p>**a<br>b**</p>');
});

check('links are shown as text and their URL, never as links', (render) => {
    assert.equal(
        render('See [Kula docs](https://example.com/a_b_c?x=1&y=2) now.'),
        '<p>See Kula docs <span class="ai-link-url" dir="ltr">(https://example.com/a_b_c?x=1&amp;y=2)</span> now.</p>',
    );
    assert.equal(render('[**bold** `code`](http://h/)'), '<p><strong>bold</strong> <code dir="ltr">code</code> <span class="ai-link-url" dir="ltr">(http://h/)</span></p>');
    assert.equal(render('[https://h/x](https://h/x)'), '<p><span class="ai-link-url" dir="ltr">https://h/x</span></p>');
    assert.equal(render('![chart](http://h/c.png)'), '<p>chart <span class="ai-link-url" dir="ltr">(http://h/c.png)</span></p>');
    // A bare URL is text, its underscores and asterisks included.
    assert.equal(render('Open https://example.com/_x_/*y* or **https://h/a_b**.'), '<p>Open https://example.com/_x_/*y* or <strong>https://h/a_b</strong>.</p>');
    assert.equal(render('[not a link] (x) and [a][b]'), '<p>[not a link] (x) and [a][b]</p>');
    for (const text of ['[x](javascript:alert(1))', '[x](javascript:alert`1`)', '[a](http://h/"onmouseover="alert(1))', '<a href="http://h">x</a>']) {
        assert.doesNotMatch(render(text), /<a[\s>]|\shref\s*=\s*"|\son\w+\s*=\s*"/i, text);
    }
});

check('<think> reasoning is a disclosure, open until it closes (L6)', (render) => {
    assert.equal(
        render('<think>\nThe user asks about **load**.\nCheck CPU.\n</think>\n\n**Answer**'),
        '<details class="ai-think"><summary>Reasoning</summary><div class="ai-think-body">The user asks about **load**.<br>Check CPU.</div></details><p><strong>Answer</strong></p>',
    );
    assert.equal(
        render('<think>\nstill thinking, `code` and <b>'),
        '<details class="ai-think" open><summary>Reasoning</summary><div class="ai-think-body">still thinking, `code` and &lt;b&gt;</div></details>',
    );
    assert.equal(render('<think>a</think>b'), '<details class="ai-think"><summary>Reasoning</summary><div class="ai-think-body">a</div></details><p>b</p>');
    assert.match(render('<think>x</think>', { reasoningLabel: 'Raisonnement <i>' }), /<summary>Raisonnement &lt;i&gt;<\/summary>/);
    // An empty pair (a model told not to reason) and a tag not yet followed by text show nothing.
    assert.equal(render('<think>\n\n</think>\n\nHello'), '<p>Hello</p>');
    assert.equal(render('<think>'), '');
    // A fence inside reasoning is reasoning text; an answer's code shows the tag as code.
    assert.equal(render('<think>\n```\nx\n</think>\ny'), '<details class="ai-think"><summary>Reasoning</summary><div class="ai-think-body">```<br>x</div></details><p>y</p>');
    assert.deepEqual(codeBlocks(render('```html\n<think>\n```')), [{ language: 'html', text: '<think>' }]);
    // Only a tag starting a line opens reasoning.
    assert.equal(render('Models write `<think>` first.'), '<p>Models write <code dir="ltr">&lt;think&gt;</code> first.</p>');
    assert.equal(render('a <think> b'), '<p>a &lt;think&gt; b</p>');
});

// ---- Untrusted input ----

check('HTML in the answer is escaped wherever it appears', (render) => {
    const payloads = [
        '<script>alert(1)</script>',
        '<img src=x onerror=alert(1)>',
        '"><svg onload=alert(1)>',
        "'; alert(1); '",
        '&lt;script&gt; &#x3C;b&#x3E; &amp;',
        '<!-- comment --><style>*{}</style>',
    ];
    for (const payload of payloads) {
        const contexts = [
            payload,
            `# ${payload}`,
            `- ${payload}\n  1. ${payload}`,
            `> ${payload}`,
            `**${payload}** *${payload}* \`${payload}\``,
            `| ${payload} | b |\n|---|---|\n| ${payload} | \`${payload}\` |`,
            `[${payload}](http://h/) [x](${payload.replace(/\s/g, '')})`,
            fenced('', payload),
            `\`\`\`${payload}\nx\n\`\`\``,
            `<think>${payload}</think>${payload}`,
        ];
        for (const text of contexts) {
            const html = render(text);
            assert.doesNotMatch(html, /<(?:script|img|svg|style|a|!--)[\s>/]|\son\w+\s*=\s*"/i, text);
        }
        // The payload is shown as written.
        assert.ok(decode(render(payload).replace(/<[^>]*>/g, '')).includes(payload), payload);
        // A code block holds the payload exactly, ready to copy.
        assert.deepEqual(codeBlocks(render(fenced('html', payload))), [{ language: 'html', text: payload }]);
    }
    assert.equal(render('a & b < c > d " e \' f'), '<p>a &amp; b &lt; c &gt; d &quot; e &#39; f</p>');
});

check('a fence language is a plain word or nothing', (render) => {
    const language = (info) => codeBlocks(render(`\`\`\`${info}\nx\n\`\`\``))[0].language;
    assert.equal(language('c++'), 'c++');
    assert.equal(language('objective-c x'), 'objective-c');
    assert.equal(language('Shell_Session'), 'Shell_Session');
    assert.equal(language('"><script>alert(1)</script>'), '');
    assert.equal(language('bash" onclick="x'), '');
    assert.equal(language('c#'), '');
    assert.equal(language('a'.repeat(32)), 'a'.repeat(32));
    assert.equal(language('a'.repeat(33)), '');
    assert.equal(language('{.python}'), '');
});

check('the placeholders the renderer uses cannot be forged', (render) => {
    const forged = ['\uE0000\uE001', '\uE000 0 \uE001', '\uE0001\uE001\uE0002\uE001', '\uE000', '\uE001', '\uE000999\uE001'];
    for (const token of forged) {
        for (const text of [
            `\`code\` ${token} **${token}** [${token}](http://h/${token}) *x*`,
            `| \`a\` | ${token} |\n|---|---|\n| ${token} | \`b\` |`,
            `- \`a\` ${token}\n# ${token} \`h\``,
        ]) {
            const html = render(text);
            const spans = (text.match(/`[ab]?(?:code|h)?`/g) || []).length;
            assert.equal((html.match(/<code dir="ltr">/g) || []).length, spans, `${JSON.stringify(text)} → ${html}`);
            assert.doesNotMatch(html, /undefined|[\uE000\uE001]/, JSON.stringify(text));
        }
    }
    // Code keeps them: it is copied as the model wrote it.
    const code = 'a \uE0000\uE001 b';
    assert.deepEqual(codeBlocks(render(fenced('', code))).map((block) => block.text), [code]);
    assert.equal(render(`\`${code}\` \`x\``), `<p><code dir="ltr">${code}</code> <code dir="ltr">x</code></p>`);
});

check('every prefix of a streamed answer renders well-formed markup', (render) => {
    const answer = [
        '<think>',
        'The user wants the memory status. Check **free** first.',
        '</think>',
        '',
        '## Memory',
        '',
        'Usage is *high*: 91 % of `16 GiB`, see [docs](https://kernel.org/doc/mm_x).',
        '',
        '| Metric | Value |',
        '|:--|--:|',
        '| used | **14.6 GiB** |',
        '| swap_used | 0 |',
        '',
        '1. Check the top processes:',
        '   ```bash',
        '   ps aux --sort=-%mem | head',
        '   # the first lines matter',
        '   ```',
        '2. Then:',
        '   - drop caches',
        '   - or add swap',
        '',
        '> **Note**: _restart_ only if needed.',
        '',
        '---',
        'Done.',
    ].join('\n');
    for (let end = 0; end <= answer.length; end++) render(answer.slice(0, end));
    const html = render(answer);
    assert.deepEqual(codeBlocks(html), [{ language: 'bash', text: 'ps aux --sort=-%mem | head\n# the first lines matter' }]);
    assert.match(html, /^<details class="ai-think">/);
    assert.match(html, /<hr><p>Done\.<\/p>$/);
});

check('random markdown-like input renders well-formed markup', (render) => {
    const tokens = [
        '*', '**', '_', '__', '`', '``', '```', '~~~', '\n', '\n\n', ' ', '  ', '   ', '\t', '#', '## ', '- ', '* ', '1. ',
        '2) ', '> ', '|', '| ', '|---|', '|:-:|', ':', '-', '---', '[', ']', '(', ')', '](', '![', 'http://h/a_b',
        '<think>', '</think>', '<br>', '<', '>', '&', '"', "'", '\\', 'a', 'b_c', 'x y', '\uE000', '\uE001', '0',
        '\u2028', 'é', '😀',
    ];
    let seed = 0x6b756c61;
    const random = () => { // mulberry32: the same inputs on every run
        seed = (seed + 0x6d2b79f5) | 0;
        let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
        t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
        return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
    for (let n = 0; n < 3000; n++) {
        let text = '';
        for (let k = 1 + Math.floor(random() * 80); k > 0; k--) text += tokens[Math.floor(random() * tokens.length)];
        try {
            render(text);
        } catch (err) {
            err.message += `\ninput: ${JSON.stringify(text)}`;
            throw err;
        }
    }
});

// ---- Cost ----

const LIMIT = 32 * 1024; // AI_MAX_ANSWER: the longest answer the panel shows
const BUDGET_MS = 50;
/** fill returns LIMIT characters: prefix, unit repeated, suffix. */
const fill = (unit, prefix = '', suffix = '') => {
    const room = LIMIT - prefix.length - suffix.length;
    return prefix + unit.repeat(Math.ceil(room / unit.length)).slice(0, room) + suffix;
};

const DENSE = [
    '<think>\nThe user asks about **load**; check `uptime` and _iowait_.\n</think>',
    '## Summary — host_01',
    'CPU is **busy** (*92 %*), __iowait__ is _low_, see [the docs](https://example.com/a_b?x=1&y=2) and https://example.com/x_y.',
    '| Metric | **Value** | Trend |',
    '|:--|--:|:-:|',
    '| `cpu_usage` | *92* % | up |',
    '| mem_used | 14.6 GiB | \\| flat |',
    '1. First **step** with `code` and *emphasis*',
    '   - nested _item_ with [link](http://h/p_q)',
    '     - deeper **bold** `x*y`',
    '2. Second step',
    '```bash',
    '# check memory pressure',
    'echo "**not bold**" `date` | grep -E "a|b"',
    '---',
    '```',
    '> **Note**: 2 * 3 * 4 and snake_case_name stay as they are.',
    '',
    '***',
    '',
].join('\n');

const ADVERSARIAL = {
    'dense mixed markdown': fill(DENSE),
    'asterisks': fill('*'),
    'underscores': fill('_'),
    'backticks': fill('`'),
    'pipes': fill('|'),
    'brackets': fill('['),
    '<think>': fill('<think>'),
    '<think> pairs': fill('<think>x</think>'),
    '<think> lines': fill('<think>\n'),
    'bold openers': fill('**a '),
    'italic openers': fill('*a '),
    'underscore openers': fill('_a '),
    'double underscore openers': fill('__a '),
    'mixed openers': fill('**_*`[a](b) '),
    'nested emphasis': fill('***a '),
    'bold over single asterisks': fill('a*a', '**', '**'),
    'bold without a closer': fill('a*a', '**'),
    'underscore bold without a closer': fill('a_a', '__'),
    'backtick runs of every length': Array.from({ length: 300 }, (_, k) => '`'.repeat(k + 1)).join(' ').slice(0, LIMIT),
    'unpaired backtick runs': fill('`a ``b ```c '),
    'escaped backticks': fill('\\`'),
    'backslashes': fill('\\'),
    'link openers': fill('[a]('),
    'link labels': fill('[a'),
    'link URLs without an end': fill('b', '[a]('),
    'URLs': fill('http://'),
    'a URL of dots': fill('.', 'http://'),
    'hashes': fill('#'),
    'heading spaces': fill(' ', '# ', 'x'),
    'closing hashes': fill('# '),
    'dashes': fill('- '),
    'nested list markers': fill('- ', '', 'x'),
    'quote markers': fill('>'),
    'nested quotes': fill('> '),
    'nested quote lines': fill('> > > > > > > > > > a\n'),
    'list items': fill('- a\n'),
    'ordered items': fill('1. a\n'),
    'a lazy list': fill('b\n', '- a\n'),
    'deep list': Array.from({ length: 200 }, (_, k) => `${' '.repeat(2 * k)}- a`).join('\n').slice(0, LIMIT),
    'indented lines': fill('  - a\n    b\n'),
    'table rows': fill('| x | `y` | *z* |\n', '| a | b | c |\n|:-|-:|:-:|\n'),
    'a table row of pipes': fill('|', '| a |\n|---|\n'),
    'pipe lines': fill('|\n'),
    'separator lines': fill('|---|\n'),
    'fences': fill('```\n'),
    'fence openers': fill('```a\n'),
    'tilde fences': fill('~~~~\n'),
    'an unclosed fence': fill('# x **y**\n', '```\n'),
    'blank lines': fill('\n'),
    'spaces': fill(' '),
    'tabs': fill('\t'),
    'paragraph lines': fill('a\n'),
    'dash lines under text': fill('a\n---\n'),
    '<br> tags': fill('<br>'),
    'placeholders': fill('\uE0001\uE001'),
    'entities': fill('&<>"\''),
};

check('rendering 32 KiB of dense or adversarial input stays well under 50 ms', (render, renderMarkdown) => {
    for (const [name, text] of Object.entries(ADVERSARIAL)) {
        assert.ok(text.length <= LIMIT && text.length > LIMIT / 2, `${name}: ${text.length} characters`);
        render(text); // checks the markup, and warms up
        let best = Infinity;
        for (let run = 0; run < 3; run++) {
            const start = performance.now();
            renderMarkdown(text);
            best = Math.min(best, performance.now() - start);
        }
        assert.ok(best < BUDGET_MS, `${name}: ${best.toFixed(1)} ms`);
    }
});
