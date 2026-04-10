/* ============================================================
   gauges.js — Bar gauge rendering, sparkline backgrounds,
   and live gauge value updates.
   ============================================================ */
'use strict';
import { colors } from './state.js';
import { formatMbps } from './utils.js';

// ---- Sparkline History (network gauges only) ----
const SPARKLINE_MAX_POINTS = 60;
const sparklineData = {
    dl: [],
    ul: [],
};

const sparklineColors = {
    dl: colors.blue,
    ul: colors.pink,
};

function drawSparkline(card, key, value, max) {
    if (!card) return;
    let canvas = card.querySelector('.sparkline-canvas');
    if (!canvas) {
        canvas = document.createElement('canvas');
        canvas.className = 'sparkline-canvas';
        card.insertBefore(canvas, card.firstChild);
    }

    const buf = sparklineData[key];
    buf.push({ v: value, m: max });
    if (buf.length > SPARKLINE_MAX_POINTS) buf.shift();

    const rect = card.getBoundingClientRect();
    const dpr = window.devicePixelRatio || 1;
    const w = rect.width;
    const h = rect.height;
    if (w === 0 || h === 0) return;

    if (canvas.width !== w * dpr || canvas.height !== h * dpr) {
        canvas.width = w * dpr;
        canvas.height = h * dpr;
        canvas.style.width = w + 'px';
        canvas.style.height = h + 'px';
    }

    const ctx = canvas.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);

    if (buf.length < 2) return;

    // Auto-scale y-axis to rolling max for network gauges
    let yMax = 0;
    for (let i = 0; i < buf.length; i++) {
        if (buf[i].v > yMax) yMax = buf[i].v;
    }
    yMax = Math.max(yMax * 1.2, 0.01); // 20% headroom

    const padding = 4;
    const graphH = h - padding * 2;
    const stepX = (w - padding * 2) / (SPARKLINE_MAX_POINTS - 1);
    const startX = padding + (SPARKLINE_MAX_POINTS - buf.length) * stepX;

    const color = sparklineColors[key];

    // Draw filled area + line
    ctx.beginPath();
    for (let i = 0; i < buf.length; i++) {
        const x = startX + i * stepX;
        const y = padding + graphH - (Math.min(buf[i].v / yMax, 1) * graphH);
        if (i === 0) ctx.moveTo(x, y);
        else ctx.lineTo(x, y);
    }

    // Fill under the line
    const fillPath = new Path2D();
    for (let i = 0; i < buf.length; i++) {
        const x = startX + i * stepX;
        const y = padding + graphH - (Math.min(buf[i].v / yMax, 1) * graphH);
        if (i === 0) fillPath.moveTo(x, y);
        else fillPath.lineTo(x, y);
    }
    fillPath.lineTo(startX + (buf.length - 1) * stepX, h);
    fillPath.lineTo(startX, h);
    fillPath.closePath();

    ctx.save();
    ctx.globalAlpha = 0.08;
    ctx.fillStyle = color;
    ctx.fill(fillPath);
    ctx.restore();

    // Stroke the line
    ctx.strokeStyle = color;
    ctx.lineWidth = 1.5;
    ctx.globalAlpha = 0.25;
    ctx.stroke();
    ctx.globalAlpha = 1;
}

// ---- Bar Gauge Drawing (alternative layout) ----
export function drawBarGauge(containerId, value, max, color) {
    const container = document.getElementById(containerId);
    if (!container) return;
    const pct = max > 0 ? Math.min((value / max) * 100, 100) : 0;
    let fill = container.querySelector('.bar-gauge-fill');
    if (!fill) {
        container.innerHTML = `<div class="bar-gauge-container"><div class="bar-gauge-track"><div class="bar-gauge-fill"></div></div></div>`;
        fill = container.querySelector('.bar-gauge-fill');
    }
    fill.style.width = pct + '%';
    // Set gradient
    if (Array.isArray(color)) {
        fill.style.background = `linear-gradient(90deg, ${color.join(', ')})`;
    } else {
        fill.style.background = color;
    }
}

export function updateGauges(sample) {
    const cpuPct = sample.cpu?.total?.usage || 0;
    const ramPct = sample.mem?.used_pct || 0;
    const swapPct = sample.swap?.used_pct || 0;
    const lavg = sample.lavg?.load1 || 0;
    const numCores = (sample.cpu?.num_cores || 1);

    // Sum network across non-lo interfaces
    let dlMbps = 0, ulMbps = 0;
    if (sample.net?.ifaces) {
        sample.net.ifaces.forEach(i => {
            if (i.name !== 'lo') { dlMbps += i.rx_mbps || 0; ulMbps += i.tx_mbps || 0; }
        });
    }

    drawBarGauge('gauge-cpu-canvas', cpuPct, 100, [colors.green, colors.yellow, colors.red]);
    document.getElementById('gauge-cpu-value').textContent = cpuPct.toFixed(1) + '%';

    drawBarGauge('gauge-ram-canvas', ramPct, 100, [colors.cyan, colors.blue, colors.purple]);
    document.getElementById('gauge-ram-value').textContent = ramPct.toFixed(1) + '%';

    drawBarGauge('gauge-swap-canvas', swapPct, 100, [colors.teal, colors.orange, colors.red]);
    document.getElementById('gauge-swap-value').textContent = swapPct.toFixed(1) + '%';

    drawBarGauge('gauge-lavg-canvas', lavg, numCores * 2, [colors.green, colors.yellow, colors.red]);
    document.getElementById('gauge-lavg-value').textContent = lavg.toFixed(2);

    const maxNet = Math.max(dlMbps, ulMbps, 1);
    drawBarGauge('gauge-dl-canvas', dlMbps, Math.max(maxNet * 1.5, 10), [colors.cyan, colors.blue]);
    document.getElementById('gauge-dl-value').textContent = formatMbps(dlMbps);

    drawBarGauge('gauge-ul-canvas', ulMbps, Math.max(maxNet * 1.5, 10), [colors.pink, colors.purple]);
    document.getElementById('gauge-ul-value').textContent = formatMbps(ulMbps);

    // Draw sparklines in network gauge backgrounds only
    drawSparkline(document.getElementById('gauge-dl'), 'dl', dlMbps, Math.max(maxNet * 1.5, 10));
    drawSparkline(document.getElementById('gauge-ul'), 'ul', ulMbps, Math.max(maxNet * 1.5, 10));
}
