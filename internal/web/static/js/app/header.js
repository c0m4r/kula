/* ============================================================
   header.js — Header bar and chart subtitle updates.
   ============================================================ */
'use strict';
import { state, escapeHTML } from './state.js';
import { formatBytesShort, formatMbps, formatPPS } from './utils.js';

export function updateHeader(s) {
    const el = (id) => document.getElementById(id);
    if (s.sys?.uptime_human) el('uptime').textContent = '⏱ ' + s.sys.uptime_human;
    el('clock').textContent = new Date(s.ts).toLocaleTimeString();

    // System info footer — with colored clock sync
    const sysInfo = [];
    if (s.sys?.clock_synced !== undefined) {
        const synced = s.sys.clock_synced;
        const cls = synced ? 'clock-synced' : 'clock-not-synced';
        const label = synced ? '✓ synced' : '✗ not synced';
        sysInfo.push(`clock: <span class="${cls}">${label}</span>`);
    }
    if (s.sys?.clock_source) sysInfo.push('source: ' + escapeHTML(s.sys.clock_source));
    if (s.sys?.entropy) sysInfo.push('entropy: ' + escapeHTML(s.sys.entropy));
    if (s.sys?.user_count !== undefined) sysInfo.push('users: ' + escapeHTML(s.sys.user_count));
    if (s.self) sysInfo.push('self: ' + s.self.cpu_pct.toFixed(1) + '% cpu, ' + formatBytesShort(s.self.mem_rss) + ' rss');
    el('sys-info').innerHTML = sysInfo.map(text => `<span class="sys-info-item">${text}</span>`).join('<span class="sys-sep mobile-hidden">│</span>');
}

export function updateSubtitles(s) {
    const el = (id, text) => { const e = document.getElementById(id); if (e) e.textContent = text; };

    if (s.cpu?.total) {
        el('cpu-subtitle', `usr:${(s.cpu.total.user || 0).toFixed(1)}% sys:${(s.cpu.total.system || 0).toFixed(1)}% io:${(s.cpu.total.iowait || 0).toFixed(1)}% ${s.cpu.num_cores || 0} cores`);
    }
    if (s.lavg) el('lavg-subtitle', `${(s.lavg.load1 || 0).toFixed(2)} / ${(s.lavg.load5 || 0).toFixed(2)} / ${(s.lavg.load15 || 0).toFixed(2)}`);
    // Memory — with % appended
    if (s.mem) {
        const memPct = s.mem.used_pct !== undefined ? s.mem.used_pct.toFixed(1) : '0.0';
        el('mem-subtitle', `${formatBytesShort(s.mem.used)} / ${formatBytesShort(s.mem.total)} (${memPct}%)`);
    }
    // Swap — with % appended
    if (s.swap) {
        const swapPct = s.swap.used_pct !== undefined ? s.swap.used_pct.toFixed(1) : '0.0';
        el('swap-subtitle', `${formatBytesShort(s.swap.used || 0)} / ${formatBytesShort(s.swap.total || 0)} (${swapPct}%)`);
    }
    if (s.net?.ifaces) {
        let rx = 0, tx = 0, rxPps = 0, txPps = 0;
        const iface = s.net.ifaces.find(i => i.name === state.selectedNet);
        if (iface) {
            rx = iface.rx_mbps || 0; tx = iface.tx_mbps || 0;
            rxPps = iface.rx_pps || 0; txPps = iface.tx_pps || 0;
        } else if (!state.selectedNet) {
            s.net.ifaces.forEach(i => {
                if (i.name !== 'lo') {
                    rx += i.rx_mbps || 0; tx += i.tx_mbps || 0;
                    rxPps += i.rx_pps || 0; txPps += i.tx_pps || 0;
                }
            });
        }
        el('net-subtitle', `↓${formatMbps(rx)} ↑${formatMbps(tx)}`);
        el('pps-subtitle', `↓${formatPPS(rxPps)} ↑${formatPPS(txPps)}`);
    }
    if (s.net?.sockets) {
        const errs = (s.net?.tcp?.in_errs_ps || 0).toFixed(2);
        const rsts = (s.net?.tcp?.out_rsts_ps || 0).toFixed(2);
        el('conn-subtitle', `estab:${s.net?.tcp?.curr_estab || 0} err:${errs}/s rst:${rsts}/s`);
    }
    if (s.disk?.devices) {
        let r = 0, w = 0, rIops = 0, wIops = 0;
        const d = s.disk.devices.find(d => d.name === state.selectedDiskIo);
        if (d) {
            r = d.read_bps || 0; w = d.write_bps || 0;
            rIops = d.reads_ps || 0; wIops = d.writes_ps || 0;
            el('diskio-subtitle', `R:${formatBytesShort(r)}/s W:${formatBytesShort(w)}/s  rIOPS:${rIops.toFixed(0)} wIOPS:${wIops.toFixed(0)}`);
        } else if (!state.selectedDiskIo) {
            s.disk.devices.forEach(d => {
                r += d.read_bps || 0; w += d.write_bps || 0;
                rIops += d.reads_ps || 0; wIops += d.writes_ps || 0;
            });
            el('diskio-subtitle', `R:${formatBytesShort(r)}/s W:${formatBytesShort(w)}/s  rIOPS:${rIops.toFixed(0)} wIOPS:${wIops.toFixed(0)}`);
        }

        let temp = 0;
        const dt = s.disk.devices.find(d => d.name === state.selectedDiskTemp);
        if (dt) {
            temp = dt.temp || 0;
            if (temp > 0) {
                el('disktemp-subtitle', `${temp.toFixed(1)}°C`);
            } else {
                el('disktemp-subtitle', '');
            }
        } else if (!state.selectedDiskTemp) {
            el('disktemp-subtitle', '');
        }
    }
    if (s.disk?.filesystems) {
        let used = 0, total = 0;
        const fs = s.disk.filesystems.find(f => f.mount === state.selectedDiskSpace);
        if (fs) {
            used = fs.used || 0; total = fs.total || 0;
        } else if (!state.selectedDiskSpace) {
            s.disk.filesystems.forEach(f => {
                used += f.used || 0;
                total += f.total || 0;
            });
        }
        if (total > 0) {
            const pct = (used / total) * 100;
            el('diskspace-subtitle', `${formatBytesShort(used)} / ${formatBytesShort(total)} (${pct.toFixed(1)}%)`);
        }
    }
    if (s.proc) el('proc-subtitle', `${s.proc.total || 0} total, ${s.proc.running || 0} running`);
    if (s.self) el('self-subtitle', `${(s.self.cpu_pct || 0).toFixed(1)}% cpu, ${formatBytesShort(s.self.mem_rss || 0)} rss, ${s.self.fds || 0} fds`);

    if (s.gpu && s.gpu.length > 0) {
        const g = s.gpu.find(g => g.name === state.selectedGpuLoad) || s.gpu[0];
        if (g) {
            const parts = [];
            if (g.load_pct > 0) parts.push(`${g.load_pct.toFixed(1)}% load`);
            if (g.power_w > 0) parts.push(`${g.power_w.toFixed(1)}W`);
            el('gpuload-subtitle', parts.length > 0 ? parts.join(', ') : 'No metrics available');
            if (g.vram_total > 0) {
                el('vram-subtitle', `${formatBytesShort(g.vram_used || 0)} / ${formatBytesShort(g.vram_total)} (${(g.vram_pct || 0).toFixed(1)}%)`);
            }
            if (g.temp > 0) {
                el('gputemp-subtitle', `${(g.temp || 0).toFixed(1)}°C`);
            }
        }
    } else {
        el('gpuload-subtitle', '');
        el('vram-subtitle', '');
        el('gputemp-subtitle', '');
    }
}
