/* ============================================================
   gauges.js — Bar gauge rendering and live gauge value updates.
   ============================================================ */
'use strict';
import { colors } from './state.js';
import { formatMbps } from './utils.js';

// ---- Bar Gauge Drawing (alternative layout) ----
export function drawBarGauge(containerId, value, max, color) {
    const container = document.getElementById(containerId);
    if (!container) return;
    const pct = Math.min((value / max) * 100, 100);
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
    const cpuTemp = sample.cpu?.temp || 0;
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
    const tempEl = document.getElementById('gauge-cpu-temp');
    if (tempEl) {
        if (cpuTemp > 0) {
            tempEl.classList.remove('hidden');
            tempEl.textContent = cpuTemp.toFixed(1) + '°C';
            if (cpuTemp >= 85) tempEl.style.color = colors.red;
            else if (cpuTemp >= 70) tempEl.style.color = colors.orange;
            else tempEl.style.color = 'var(--text-muted)';
        }
    }

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
}
