/* ============================================================
   chart-image.js — PNG snapshots of chart cards. The rendered
   Chart.js canvas is composited with the card title, header
   details, hostname and visible time range on the opaque theme
   background, at the canvas's own device-pixel resolution.
   ============================================================ */
'use strict';

const PADDING = 16;
const ROW_GAP = 10;
const LINE_HEIGHT = 1.35;
const DETAIL_SEPARATOR = ' · ';
const RANGE_SEPARATOR = ' – ';
const ELLIPSIS = '…';
const SLUG_MAX_CHARS = 80;

/**
 * Filename-safe slug. Letters and digits of every script are kept, so
 * translated chart titles do not all collapse into the fallback.
 */
export function chartImageSlug(text, fallback = 'chart') {
    const chars = Array.from(String(text ?? '')
        .normalize('NFKC')
        .toLowerCase()
        .replace(/[^\p{L}\p{N}]+/gu, '-')
        .replace(/^-+|-+$/g, ''));
    const slug = chars.slice(0, SLUG_MAX_CHARS).join('').replace(/-+$/, '');
    return slug || fallback;
}

function twoDigits(value) {
    return String(value).padStart(2, '0');
}

/** kula[-host]-<chart>-<YYYYMMDD-HHMMSS>[Z].png in the dashboard's time zone. */
export function chartImageFilename({ title, hostname, date = new Date(), utc = false } = {}) {
    const part = utc
        ? [date.getUTCFullYear(), date.getUTCMonth() + 1, date.getUTCDate(),
            date.getUTCHours(), date.getUTCMinutes(), date.getUTCSeconds()]
        : [date.getFullYear(), date.getMonth() + 1, date.getDate(),
            date.getHours(), date.getMinutes(), date.getSeconds()];
    const stamp = `${part[0]}${part.slice(1, 3).map(twoDigits).join('')}-` +
        `${part.slice(3).map(twoDigits).join('')}${utc ? 'Z' : ''}`;
    const host = chartImageSlug(hostname, '');
    return ['kula', host, chartImageSlug(title), stamp].filter(Boolean).join('-') + '.png';
}

/** Longest prefix of `text` (plus an ellipsis) that fits `maxWidth`. */
export function fitText(ctx, text, maxWidth) {
    if (!text || !(maxWidth > 0)) return '';
    if (ctx.measureText(text).width <= maxWidth) return text;
    const chars = Array.from(text);
    let low = 0;
    let high = chars.length - 1;
    while (low < high) {
        const middle = (low + high + 1) >> 1;
        const candidate = chars.slice(0, middle).join('').trimEnd() + ELLIPSIS;
        if (ctx.measureText(candidate).width <= maxWidth) low = middle;
        else high = middle - 1;
    }
    return low > 0 ? chars.slice(0, low).join('').trimEnd() + ELLIPSIS : '';
}

function rowHeight(...fonts) {
    return Math.ceil(Math.max(...fonts.map(font => font.size)) * LINE_HEIGHT);
}

// Footer labels share one row when they fit; otherwise each gets its own
// start-aligned row so neither the hostname nor the time range is lost.
function footerRows(ctx, start, end, width, gap) {
    if (start && end && ctx.measureText(start).width + gap + ctx.measureText(end).width <= width) {
        return [[{ text: start, edge: 'start' }, { text: end, edge: 'end' }]];
    }
    return [start, end]
        .map(text => fitText(ctx, text, width))
        .filter(Boolean)
        .map(text => [{ text, edge: 'start' }]);
}

/**
 * Paint a chart snapshot onto `target` and size it. Layout is in CSS
 * pixels, scaled by `spec.ratio` so the source bitmap is copied 1:1.
 *
 * spec: { source, ratio, rtl, title, details, footerStart, footerEnd,
 *         background: [colors painted in order], border,
 *         fonts: { title, detail, footer: { font, size, color } } }
 */
export function paintChartImage(target, spec) {
    const ratio = spec.ratio > 0 ? spec.ratio : 1;
    const chartWidth = spec.source.width / ratio;
    const chartHeight = spec.source.height / ratio;
    const { title: titleFont, detail: detailFont, footer: footerFont } = spec.fonts;
    const width = Math.ceil(chartWidth + PADDING * 2);
    const contentWidth = width - PADDING * 2;
    const ctx = target.getContext('2d');

    // Measure before sizing: resizing the canvas resets the context state.
    ctx.font = footerFont.font;
    const footer = footerRows(ctx, spec.footerStart, spec.footerEnd, contentWidth, footerFont.size * 1.5);

    const headerHeight = rowHeight(titleFont, detailFont);
    const footerRowHeight = rowHeight(footerFont);
    const dividerY = PADDING + headerHeight + ROW_GAP;
    const chartY = dividerY + 1 + ROW_GAP;
    const footerY = chartY + chartHeight + ROW_GAP;
    const height = Math.ceil((footer.length ? footerY + footer.length * footerRowHeight : chartY + chartHeight) +
        PADDING);

    target.width = Math.round(width * ratio);
    target.height = Math.round(height * ratio);
    ctx.setTransform(ratio, 0, 0, ratio, 0, 0);

    spec.background.filter(Boolean).forEach(color => {
        ctx.fillStyle = color;
        ctx.fillRect(0, 0, width, height);
    });

    // Offsets are measured from the reading-start edge, so the same layout
    // mirrors for right-to-left interfaces. Alignment is explicit because
    // not every browser honours the context's direction for start/end.
    const x = offset => (spec.rtl ? width - PADDING - offset : PADDING + offset);
    const align = edge => ((edge === 'start') !== Boolean(spec.rtl) ? 'left' : 'right');
    ctx.direction = spec.rtl ? 'rtl' : 'ltr';
    ctx.textBaseline = 'middle';

    const headerMiddle = PADDING + headerHeight / 2;
    ctx.font = titleFont.font;
    ctx.fillStyle = titleFont.color;
    ctx.textAlign = align('start');
    const title = fitText(ctx, spec.title, contentWidth);
    ctx.fillText(title, x(0), headerMiddle);
    const detailOffset = ctx.measureText(title).width + titleFont.size * 0.6;
    if (spec.details) {
        ctx.font = detailFont.font;
        ctx.fillStyle = detailFont.color;
        const details = fitText(ctx, spec.details, contentWidth - detailOffset);
        if (details) ctx.fillText(details, x(detailOffset), headerMiddle);
    }

    ctx.fillStyle = spec.border;
    ctx.fillRect(0, dividerY, width, 1);

    ctx.drawImage(spec.source, PADDING, chartY, chartWidth, chartHeight);

    ctx.font = footerFont.font;
    ctx.fillStyle = footerFont.color;
    footer.forEach((row, index) => {
        const middle = footerY + (index + 0.5) * footerRowHeight;
        row.forEach(({ text, edge }) => {
            ctx.textAlign = align(edge);
            ctx.fillText(text, x(edge === 'start' ? 0 : contentWidth), middle);
        });
    });
    return target;
}

function fontSpec(element) {
    const style = window.getComputedStyle(element);
    const size = parseFloat(style.fontSize) || 12;
    return {
        font: `${style.fontStyle} ${style.fontWeight} ${size}px ${style.fontFamily}`,
        size,
        color: style.color,
    };
}

// Header details and the footer use the subtitle style. Cards without a
// subtitle (split cards) resolve it through a temporary, hidden probe.
function subtitleFont(card) {
    const subtitle = card.querySelector('.chart-subtitle');
    if (subtitle) return fontSpec(subtitle);
    const probe = document.createElement('span');
    probe.className = 'chart-subtitle';
    probe.hidden = true;
    (card.querySelector('.chart-header') || card).appendChild(probe);
    try {
        return fontSpec(probe);
    } finally {
        probe.remove();
    }
}

function cardDetails(card) {
    const header = card.querySelector('.chart-header');
    if (!header) return '';
    const parts = [];
    const subtitle = header.querySelector('.chart-subtitle')?.textContent?.trim();
    if (subtitle) parts.push(subtitle);
    // A visible device selector names what the chart shows (e.g. eth0, sda).
    header.querySelectorAll('select.chart-selector:not(.hidden)').forEach(select => {
        const selected = select.selectedOptions?.[0]?.textContent?.trim();
        if (selected) parts.push(selected);
    });
    return parts.join(DETAIL_SEPARATOR);
}

function visibleRange(chart, formatTimestamp) {
    const minimum = Number(chart?.scales?.x?.min);
    const maximum = Number(chart?.scales?.x?.max);
    if (!Number.isFinite(minimum) || !Number.isFinite(maximum) || maximum < minimum) return '';
    const from = formatTimestamp(minimum);
    const to = formatTimestamp(maximum);
    return from && to ? `${from}${RANGE_SEPARATOR}${to}` : '';
}

function canvasBlob(canvas) {
    return new Promise((resolve, reject) => {
        canvas.toBlob(blob => (blob ? resolve(blob) : reject(new Error('chart image is empty'))), 'image/png');
    });
}

function downloadBlob(blob, filename) {
    const href = URL.createObjectURL(blob);
    const anchor = document.createElement('a');
    anchor.href = href;
    anchor.download = filename;
    anchor.click();
    // Images can be several megabytes; give the download time to take the
    // blob before its URL is released.
    setTimeout(() => URL.revokeObjectURL(href), 10000);
}

/**
 * Compose and download a PNG of `chart` using the styles of its `card`.
 * options: { hostname, formatTimestamp(ms), utc, rtl }
 */
export async function saveChartImage(chart, card, options = {}) {
    const source = chart?.canvas;
    if (!source || !card || source.width === 0 || source.height === 0) return null;

    const heading = card.querySelector('.chart-header h3');
    const pageStyle = window.getComputedStyle(document.body);
    const cardStyle = window.getComputedStyle(card);
    const titleFont = fontSpec(heading || card);
    const detailFont = subtitleFont(card);
    const title = heading?.textContent?.trim() || '';
    const formatTimestamp = options.formatTimestamp || (value => new Date(value).toISOString());

    const image = paintChartImage(document.createElement('canvas'), {
        source,
        ratio: chart.currentDevicePixelRatio || window.devicePixelRatio || 1,
        rtl: Boolean(options.rtl),
        title,
        details: cardDetails(card),
        footerStart: options.hostname || '',
        footerEnd: visibleRange(chart, formatTimestamp),
        // Cards are translucent over the page; stack both for the on-screen color.
        background: [
            pageStyle.getPropertyValue('--bg-primary').trim() || pageStyle.backgroundColor || '#fff',
            cardStyle.backgroundColor,
        ],
        border: pageStyle.getPropertyValue('--border-color').trim() || cardStyle.borderTopColor,
        fonts: {
            title: titleFont,
            detail: detailFont,
            footer: detailFont,
        },
    });
    const filename = chartImageFilename({
        title,
        hostname: options.hostname,
        utc: Boolean(options.utc),
    });
    downloadBlob(await canvasBlob(image), filename);
    return filename;
}
