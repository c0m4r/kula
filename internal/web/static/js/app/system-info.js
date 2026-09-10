/* Current hardware inventory has its own request lifecycle. Chart history,
   pause state, and WebSocket buffers never supply values to this page. */
import { apiUrl } from './api.js';
import { i18n } from './i18n.js';
import { formatBytesShort } from './format.js';

const sectionDefinitions = [
    { id: 'overview', index: '01' },
    { id: 'cpu', index: '02' },
    { id: 'memory', index: '03' },
    { id: 'storage', index: '04' },
    { id: 'network', index: '05' },
    { id: 'devices', index: '06' },
    { id: 'sensors', index: '07' },
];
const routeHash = '#system-info';
let active = 'overview';
let snapshot = null;
let timer = null;
let request = null;
let enabled = true;
let dashboardScroll = 0;

const el = id => document.getElementById(id);
const finite = value => typeof value === 'number' && Number.isFinite(value);
const present = value => value !== '' && value !== null && value !== undefined;

function normalizedKey(key) {
    return String(key)
        .replace(/([a-z0-9])([A-Z])/g, '$1_$2')
        .replace(/[^A-Za-z0-9]+/g, '_')
        .replace(/^_|_$/g, '')
        .toLowerCase();
}

function humanize(key) {
    const words = normalizedKey(key).replaceAll('_', ' ').replace(/^./, character => character.toUpperCase());
    return words.replace(/\b(cpu|gpu|usb|pci|uuid|wwid|mtu|iops|io|ecc|bios|numa|rpm|mhz|mbps|uefi|ip)\b/gi,
        word => word.toUpperCase());
}

const label = key => i18n.translations[`si_${normalizedKey(key)}`] || humanize(key);
const number = (value, unit = '', digits = 1) => finite(Number(value))
    ? `${Number(value).toLocaleString(i18n.currentLang, { maximumFractionDigits: digits })}${unit}` : '—';
const bytes = value => finite(Number(value)) ? formatBytesShort(Number(value)) : '—';
const percent = value => number(value, '%');

function node(tag, className, text) {
    const element = document.createElement(tag);
    if (className) element.className = className;
    if (text !== undefined) element.textContent = String(text);
    return element;
}

const iconPaths = {
    activity: ['M3 12h4l2.5-6 5 12 2.5-6h4'],
    cpu: ['M9 2v2M15 2v2M9 20v2M15 20v2M20 9h2M20 15h2M2 9h2M2 15h2',
        'M6 4h12a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2Z', 'M9 9h6v6H9Z'],
    devices: ['M4 5h16v11H4Z', 'M8 20h8M12 16v4'],
    filesystem: ['M3 6h7l2 2h9v10H3Z'],
    gpu: ['M3 6h15v12H3Z', 'M18 10h3M18 14h3', 'M7 10h7v4H7Z'],
    hardware: ['M5 4h14v16H5Z', 'M9 8h6v6H9Z', 'M8 17h.01M12 17h.01M16 17h.01'],
    memory: ['M4 6h16v12H4Z', 'M8 10v4M12 10v4M16 10v4', 'M7 18v2M11 18v2M15 18v2M19 18v2'],
    module: ['M7 3h10v18H7Z', 'M10 7h4M10 11h4M10 15h4'],
    network: ['M5 10a10 10 0 0 1 14 0', 'M8 14a6 6 0 0 1 8 0', 'M11 18a2 2 0 0 1 2 0'],
    pci: ['M4 5h16v12H4Z', 'M8 17v3M16 17v3', 'M8 9h3v4H8ZM14 8h2M14 11h2M14 14h2'],
    power: ['M4 7h14v10H4Z', 'M20 10v4', 'M7 10h5v4H7Z'],
    sensor: ['M10 14.8V5a2 2 0 0 1 4 0v9.8a4 4 0 1 1-4 0Z', 'M12 9v7'],
    server: ['M3 4h18v6H3Z', 'M3 14h18v6H3Z', 'M7 7h.01M7 17h.01M11 7h6M11 17h6'],
    storage: ['M4 6c0-1.1 3.6-2 8-2s8 .9 8 2-3.6 2-8 2-8-.9-8-2Z',
        'M4 6v6c0 1.1 3.6 2 8 2s8-.9 8-2V6', 'M4 12v6c0 1.1 3.6 2 8 2s8-.9 8-2v-6'],
    swap: ['M7 7h12l-3-3M19 7l-3 3', 'M17 17H5l3 3M5 17l3-3'],
    usb: ['M12 3v13', 'M9 6l3-3 3 3', 'M12 10H8l-2 2', 'M12 13h4l2-2', 'M6 12v3', 'M18 11v3',
        'M4.5 15h3v3h-3Z', 'M16.5 14a1.5 1.5 0 1 0 3 0 1.5 1.5 0 0 0-3 0Z', 'M12 16a2 2 0 1 0 0 4 2 2 0 0 0 0-4Z'],
};

function icon(name, className) {
    const wrap = node('span', className);
    wrap.dataset.icon = name;
    wrap.setAttribute('aria-hidden', 'true');
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.setAttribute('fill', 'none');
    svg.setAttribute('stroke', 'currentColor');
    svg.setAttribute('stroke-width', '1.8');
    svg.setAttribute('stroke-linecap', 'round');
    svg.setAttribute('stroke-linejoin', 'round');
    svg.setAttribute('focusable', 'false');
    for (const pathData of iconPaths[name] || iconPaths.hardware) {
        const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
        path.setAttribute('d', pathData);
        svg.append(path);
    }
    wrap.append(svg);
    return wrap;
}

function isOnline(value) {
    return ['up', 'online', '1', 'connected'].includes(String(value ?? '').toLowerCase());
}

function translatedState(value) {
    const state = String(value ?? '').toLowerCase();
    if (isOnline(state)) return label('online');
    if (['down', 'offline', '0', 'disconnected'].includes(state)) return label('offline');
    return value;
}

function displayValue(key, value) {
    if (!present(value)) return '—';
    if (Array.isArray(value)) return value.filter(present).join(', ') || '—';
    if (typeof value === 'boolean') return label(value ? 'yes' : 'no');
    const name = normalizedKey(key);
    const numeric = Number(value);
    if (['carrier', 'online', 'read_only', 'removable', 'rotational'].includes(name) &&
        ['0', '1'].includes(String(value))) return label(String(value) === '1' ? 'yes' : 'no');
    if (name === 'state') return translatedState(value);
    if (name === 'size_mib' && Number.isFinite(numeric)) return bytes(numeric * 1024 * 1024);
    if (name === 'size_kib' && Number.isFinite(numeric)) return bytes(numeric * 1024);
    if (['speed_mts', 'configured_speed_mts'].includes(name) && Number.isFinite(numeric)) {
        return number(numeric, ' MT/s', 0);
    }
    if (name === 'configured_voltage_mv' && Number.isFinite(numeric)) return number(numeric / 1000, ' V', 2);
    if (name === 'speed_mbps' && Number.isFinite(numeric)) return number(numeric, ' Mb/s', 0);
    if (name === 'capacity_percent' && Number.isFinite(numeric)) return percent(numeric);
    if (['energy_now_uwh', 'energy_full_uwh', 'energy_design_uwh'].includes(name) && Number.isFinite(numeric)) {
        return number(numeric / 1e6, ' Wh', 2);
    }
    if (['charge_now_uah', 'charge_full_uah'].includes(name) && Number.isFinite(numeric)) {
        return number(numeric / 1e6, ' Ah', 2);
    }
    if (name === 'voltage_uv' && Number.isFinite(numeric)) return number(numeric / 1e6, ' V', 2);
    if (name === 'power_uw' && Number.isFinite(numeric)) return number(numeric / 1e6, ' W', 2);
    return String(value);
}

function select(source = {}, keys = []) {
    const selected = {};
    for (const key of keys) if (present(source?.[key])) selected[key] = source[key];
    return selected;
}

function without(source = {}, keys = []) {
    const omitted = new Set(keys);
    return Object.fromEntries(Object.entries(source || {})
        .filter(([key, value]) => !omitted.has(key) && present(value)));
}

function details(values = {}) {
    const list = node('dl', 'system-info-facts');
    for (const [key, value] of Object.entries(values || {})) {
        if (!present(value) || (typeof value === 'object' && !Array.isArray(value))) continue;
        list.append(node('dt', '', label(key)), node('dd', '', displayValue(key, value)));
    }
    if (!list.children.length) return node('p', 'system-info-empty', label('unavailable'));
    return list;
}

function card(title, values, options = {}) {
    const item = node('section', `system-info-card${options.wide ? ' system-info-wide' : ''}`);
    const heading = node('header', 'system-info-card-heading');
    if (options.icon) heading.append(icon(options.icon, 'system-info-card-icon'));
    const text = node('div');
    text.append(node('h4', '', title));
    if (options.subtitle) text.append(node('p', '', options.subtitle));
    heading.append(text);
    if (present(options.state)) {
        heading.append(node('span', `system-info-state ${isOnline(options.state) ? 'is-online' : 'is-offline'}`,
            translatedState(options.state)));
    }
    item.append(heading);
    if (values) item.append(details(values));
    return item;
}

function meter(title, value, detail) {
    const wrap = node('div', 'system-info-meter');
    const row = node('div', 'system-info-meter-label');
    row.append(node('span', '', title), node('strong', '', detail ?? percent(value)));
    wrap.append(row);
    if (finite(value)) {
        const bounded = Math.max(0, Math.min(100, value));
        const tone = bounded >= 90 ? ' is-critical' : bounded >= 75 ? ' is-warning' : '';
        const bar = node('div', `system-info-progress${tone}`);
        bar.setAttribute('role', 'progressbar');
        bar.setAttribute('aria-label', title);
        bar.setAttribute('aria-valuemin', '0');
        bar.setAttribute('aria-valuemax', '100');
        bar.setAttribute('aria-valuenow', String(bounded));
        const fill = node('div', 'system-info-progress-fill');
        fill.style.setProperty('--meter-value', `${bounded}%`);
        bar.append(fill);
        wrap.append(bar);
    }
    return wrap;
}

function expandable(key, title, child) {
    const wrap = node('details', 'system-info-expandable');
    wrap.dataset.key = key;
    wrap.append(node('summary', '', title), child);
    return wrap;
}

function appendExpandable(item, key, title, values) {
    if (!values || !Object.values(values).some(present)) return false;
    item.append(expandable(key, title, details(values)));
    return true;
}

function table(columns, rows) {
    const wrap = node('div', 'system-info-table-wrap');
    const tableElement = node('table', 'system-info-table');
    const head = node('thead');
    const headingRow = node('tr');
    for (const column of columns) {
        const cell = node('th', '', label(column));
        cell.scope = 'col';
        headingRow.append(cell);
    }
    head.append(headingRow);
    tableElement.append(head);
    const body = node('tbody');
    for (const row of rows) {
        const rowElement = node('tr');
        for (const column of columns) rowElement.append(node('td', '', displayValue(column, row[column])));
        body.append(rowElement);
    }
    tableElement.append(body);
    wrap.append(tableElement);
    return wrap;
}

function statPair(items) {
    const wrap = node('div', 'system-info-stat-pair');
    for (const [title, value] of items) {
        const stat = node('div', 'system-info-stat');
        stat.append(node('span', '', title), node('strong', '', value));
        wrap.append(stat);
    }
    return wrap;
}

function callout(text) {
    return node('p', 'system-info-note system-info-wide', text);
}

function summaryCard(iconName, title, value, detail, usage, usageLabel = label('used')) {
    const item = node('article', 'system-info-summary-card');
    const top = node('div', 'system-info-summary-top');
    top.append(icon(iconName, 'system-info-summary-icon'), node('span', 'system-info-summary-title', title));
    item.append(top, node('strong', 'system-info-summary-value', value),
        node('span', 'system-info-summary-detail', detail));
    if (finite(usage)) item.append(meter(usageLabel, usage));
    return item;
}

function profile(data) {
    const system = data.system || {};
    const item = node('section', 'system-info-profile system-info-wide');
    const primary = node('div');
    primary.append(node('span', 'system-info-profile-label', label('this_server')),
        node('h4', '', system.hostname || label('unnamed_system')));
    const description = [system.manufacturer, system.product].filter(present).join(' · ') ||
        label('hardware_identity_unavailable');
    primary.append(node('p', 'system-info-profile-description', description));
    const chips = node('div', 'system-info-chips');
    for (const value of [
        system.os,
        system.architecture,
        system.hypervisor ? `${label('virtualized_with')} ${system.hypervisor}` : system.firmware,
    ]) {
        if (present(value)) chips.append(node('span', 'system-info-chip', value));
    }
    primary.append(chips);
    const side = node('div', 'system-info-profile-side');
    side.append(node('span', '', label('uptime')),
        node('strong', '', data.live?.sys?.uptime_human || '—'),
        node('span', '', label('since_last_restart')));
    item.append(primary, side);
    return item;
}

function mainFilesystem(data) {
    return data.filesystems?.find(filesystem => filesystem.mount === '/' && filesystem.usage) ||
        data.filesystems?.find(filesystem => filesystem.usage);
}

function primaryInterface(data) {
    return data.network?.find(iface => iface.name !== 'lo' && iface.details?.state === 'up') ||
        data.network?.find(iface => iface.name !== 'lo') || data.network?.[0];
}

function overview(data, grid) {
    grid.append(profile(data));
    const live = data.live;
    const filesystem = mainFilesystem(data);
    const iface = primaryInterface(data);
    const cpu = data.cpu || {};
    const summaries = node('section', 'system-info-summary-grid system-info-wide');
    const coreCount = cpu.cores || cpu.logical_cpus;
    summaries.append(
        summaryCard('cpu', label('processor'), cpu.details?.model_name || label('unavailable'),
            [
                coreCount ? `${number(coreCount, '', 0)} ${label('cores')}` : '',
                cpu.logical_cpus ? `${number(cpu.logical_cpus, '', 0)} ${label('logical_cpus')}` : '',
            ].filter(Boolean).join(' · '), live?.cpu?.total?.usage, label('cpu_usage')),
        summaryCard('memory', label('memory'), bytes(live?.mem?.total),
            live?.mem ? `${bytes(live.mem.used)} ${label('used').toLowerCase()} · ${bytes(live.mem.available)} ${label('available').toLowerCase()}`
                : label('waiting_for_metrics'), live?.mem?.used_pct, label('memory_used')),
        summaryCard('storage', label('main_storage'), bytes(filesystem?.usage?.total),
            filesystem?.usage ? `${bytes(filesystem.usage.used)} ${label('used').toLowerCase()} · ${filesystem.mount}`
                : label('storage_usage_unavailable'), filesystem?.usage?.used_pct, label('space_used')),
        summaryCard('network', label('network'), iface?.name || label('unavailable'),
            iface ? [
                finite(iface.speed_mbps) ? number(iface.speed_mbps, ' Mb/s', 0) : '',
                iface.addresses?.[0],
            ].filter(Boolean).join(' · ') || label('link_details_unavailable') : label('no_interfaces')),
    );
    grid.append(summaries);

    const system = data.system || {};
    const identity = card(label('system_and_software'), {
        os: system.os,
        kernel: system.kernel,
        architecture: system.architecture,
        firmware: system.firmware,
        hypervisor: system.hypervisor,
    }, { icon: 'server' });
    appendExpandable(identity, 'system-identifiers', label('system_identifiers'),
        without(system, ['hostname', 'os', 'kernel', 'architecture', 'manufacturer', 'product', 'firmware', 'hypervisor']));
    grid.append(identity);

    const runtime = live ? card(label('current_activity'), {
        clock_synced: live.sys?.clock_synced,
        signed_in_users: live.sys?.user_count,
        total_processes: live.proc?.total,
        running_processes: live.proc?.running,
        load_average: [live.lavg?.load1, live.lavg?.load5, live.lavg?.load15]
            .filter(finite).map(value => number(value)).join(' / '),
    }, { icon: 'activity' }) : card(label('current_activity'),
        { status: label('waiting_for_metrics') }, { icon: 'activity' });
    if (live) appendExpandable(runtime, 'runtime-details', label('technical_details'), {
        threads: live.proc?.threads,
        blocked_processes: live.proc?.blocked,
        zombie_processes: live.proc?.zombie,
        clock_source: live.sys?.clock_source,
    });
    grid.append(runtime);

    const hardware = card(label('hardware_identity'), {
        manufacturer: system.manufacturer,
        product: system.product,
        motherboard: [data.board?.manufacturer, data.board?.model].filter(present).join(' '),
        bios_firmware: [data.bios?.vendor, data.bios?.version].filter(present).join(' '),
    }, { wide: true, icon: 'hardware' });
    appendExpandable(hardware, 'board-details', label('motherboard_details'), data.board);
    appendExpandable(hardware, 'firmware-details', label('firmware_details'), data.bios);
    grid.append(hardware);
}

function cpu(data, grid) {
    const processor = data.cpu || {};
    const source = processor.details || {};
    const item = card(source.model_name || label('processor'), {
        vendor: source.vendor,
        physical_cores: processor.cores || undefined,
        logical_cpus: processor.logical_cpus || undefined,
        sockets: processor.sockets || undefined,
        numa_nodes: source.numa_nodes,
    }, { icon: 'cpu', subtitle: label('processor_overview') });
    appendExpandable(item, 'processor-details', label('technical_details'),
        without(source, ['features', 'model_name', 'vendor', 'numa_nodes']));
    grid.append(item);

    if (data.live) {
        const usage = card(label('live_processor_activity'), null,
            { icon: 'activity', subtitle: label('live_not_history') });
        usage.append(meter(label('cpu_usage'), data.live.cpu?.total?.usage), statPair([
            [label('user_apps'), percent(data.live.cpu?.total?.user)],
            [label('system_work'), percent(data.live.cpu?.total?.system)],
            [label('io_wait'), percent(data.live.cpu?.total?.iowait)],
            [label('temperature'), finite(data.live.cpu?.temp) ? number(data.live.cpu.temp, ' °C') : '—'],
        ]));
        grid.append(usage);
    }

    const more = card(label('processor_details'), null,
        { wide: true, icon: 'cpu', subtitle: label('processor_details_intro') });
    let hasMore = false;
    if (source.features) {
        hasMore = true;
        more.append(expandable('features', label('instruction_features'),
            node('p', 'system-info-feature-list', source.features)));
    }
    if (processor.caches?.length) {
        hasMore = true;
        more.append(expandable('caches', `${label('caches')} · ${processor.caches.length}`,
            table(['level', 'type', 'size', 'shared_cpus', 'line_size', 'ways'], processor.caches)));
    }
    if (processor.frequency?.length) {
        const rows = processor.frequency.map(row => ({
            ...row,
            current_mhz: Number.isFinite(Number(row.current_khz)) ? number(Number(row.current_khz) / 1000) : undefined,
            minimum_mhz: Number.isFinite(Number(row.minimum_khz)) ? number(Number(row.minimum_khz) / 1000) : undefined,
            maximum_mhz: Number.isFinite(Number(row.maximum_khz)) ? number(Number(row.maximum_khz) / 1000) : undefined,
        }));
        hasMore = true;
        more.append(expandable('frequency', `${label('frequency')} · ${rows.length}`,
            table(['cpus', 'driver', 'governor', 'current_mhz', 'minimum_mhz', 'maximum_mhz'], rows)));
    }
    if (Object.keys(processor.vulnerabilities || {}).length) {
        hasMore = true;
        more.append(expandable('vulnerabilities', label('security_mitigations'),
            details(processor.vulnerabilities)));
    }
    if (hasMore) grid.append(more);
}

function memory(data, grid) {
    if (data.live) {
        for (const [name, stats, iconName] of [
            ['memory', data.live.mem, 'memory'],
            ['swap', data.live.swap, 'swap'],
        ]) {
            const item = card(label(name), null, {
                icon: iconName,
                subtitle: name === 'memory' ? label('physical_memory') : label('overflow_memory'),
            });
            item.append(meter(label('used'), stats?.used_pct,
                stats ? `${bytes(stats.used)} / ${bytes(stats.total)} · ${percent(stats.used_pct)}` : '—'),
            statPair([
                [label('total'), bytes(stats?.total)],
                [label('available'), bytes(stats?.available ?? stats?.free)],
            ]));
            grid.append(item);
        }
    }

    for (const [index, dimm] of (data.dimms || []).entries()) {
        const title = dimm.label || dimm.slot || `${label('memory_module')} ${index + 1}`;
        const primary = ['size_mib', 'size_kib', 'type', 'configured_speed_mts', 'manufacturer',
            'part_number', 'status', 'source'];
        const item = card(title, select(dimm, primary), {
            icon: 'module',
            subtitle: dimm.bank || label('installed_memory_module'),
        });
        appendExpandable(item, `dimm-${index}`, label('technical_details'),
            without(dimm, [...primary, 'slot', 'label', 'bank']));
        grid.append(item);
    }
    if (!data.dimms?.length) grid.append(callout(label('modules_unavailable')));

    const fields = Object.fromEntries(Object.entries(data.memory || {}).map(([key, value]) => [
        key,
        /^\d+ kB$/.test(value) ? bytes(Number(value.split(' ')[0]) * 1024) : value,
    ]));
    if (Object.keys(fields).length) {
        const accounting = card(label('memory_accounting'), null, {
            wide: true,
            icon: 'memory',
            subtitle: label('memory_accounting_intro'),
        });
        accounting.append(expandable('memory-accounting', label('view_memory_breakdown'), details(fields)));
        grid.append(accounting);
    }
}

function filesystemList(filesystems) {
    const list = node('div', 'system-info-filesystems');
    for (const filesystem of filesystems) {
        const item = node('article', 'system-info-filesystem');
        const head = node('div', 'system-info-filesystem-head');
        head.append(node('strong', '', filesystem.mount || label('unknown_mount')),
            node('span', '', filesystem.usage
                ? `${percent(filesystem.usage.used_pct)} ${label('used').toLowerCase()}`
                : label('usage_unavailable')));
        const meta = node('div', 'system-info-filesystem-meta');
        for (const value of [
            filesystem.device,
            filesystem.type,
            filesystem.usage ? `${bytes(filesystem.usage.available)} ${label('available').toLowerCase()}` : '',
        ]) {
            if (present(value)) meta.append(node('span', '', value));
        }
        item.append(head, meta);
        if (filesystem.usage) {
            item.append(meter(label('space_used'), filesystem.usage.used_pct,
                `${bytes(filesystem.usage.used)} / ${bytes(filesystem.usage.total)}`));
        }
        list.append(item);
    }
    return list;
}

function driveCard(disk, index) {
    const info = disk.details || {};
    const title = info.model || info.volume_name || disk.name;
    const item = card(title, {
        capacity: disk.size_bytes === undefined ? undefined : bytes(disk.size_bytes),
        drive_type: info.type,
        mounted_at: disk.mounts?.join(', '),
    }, { icon: 'storage', subtitle: `/dev/${disk.name}` });
    if (finite(disk.busy_pct)) item.append(meter(label('drive_activity'), disk.busy_pct));
    if ([disk.read_bps, disk.write_bps, disk.reads_ps, disk.writes_ps].some(finite)) {
        item.append(statPair([
            [label('reading_now'), finite(disk.read_bps) ? `${bytes(disk.read_bps)}/s` : '—'],
            [label('writing_now'), finite(disk.write_bps) ? `${bytes(disk.write_bps)}/s` : '—'],
            [label('read_operations'), number(disk.reads_ps, '/s')],
            [label('write_operations'), number(disk.writes_ps, '/s')],
        ]));
    }
    appendExpandable(item, `drive-${index}`, label('technical_details'), {
        ...without(info, ['model', 'volume_name', 'type']),
        parent_device: disk.parent,
        backing_devices: disk.slaves?.join(', '),
        operations_in_flight: disk.in_flight,
    });
    return item;
}

function storage(data, grid) {
    const all = data.disks || [];
    const primary = all.filter(disk => disk.details?.type !== 'partition' && !disk.name.startsWith('loop'));
    const prominent = primary.length ? primary : all.slice(0, 1);
    for (const disk of prominent) grid.append(driveCard(disk, all.indexOf(disk)));
    if (!all.length) grid.append(callout(label('no_storage_devices')));

    if (data.filesystems?.length) {
        const item = card(label('mounted_storage'), null, {
            wide: true,
            icon: 'filesystem',
            subtitle: label('mounted_storage_intro'),
        });
        item.append(filesystemList(data.filesystems));
        item.append(expandable('filesystem-details', label('filesystem_technical_details'),
            table(['mount', 'device', 'type', 'options'], data.filesystems)));
        grid.append(item);
    }

    const secondary = all.filter(disk => !prominent.includes(disk));
    if (secondary.length) {
        const item = card(label('partitions_and_virtual_devices'), null, {
            wide: true,
            icon: 'devices',
            subtitle: label('secondary_storage_intro'),
        });
        item.append(expandable('secondary-storage', `${label('view_devices')} · ${secondary.length}`,
            table(['name', 'type', 'capacity', 'parent', 'mounts'], secondary.map(disk => ({
                name: disk.name,
                type: disk.details?.type,
                capacity: bytes(disk.size_bytes),
                parent: disk.parent,
                mounts: disk.mounts?.join(', '),
            })))));
        grid.append(item);
    }
    grid.append(callout(label('storage_note')));
}

function interfaceCard(iface, index) {
    const state = iface.details?.state;
    const item = card(iface.name, {
        ip_addresses: iface.addresses?.join('\n'),
        link_speed: finite(iface.speed_mbps) ? number(iface.speed_mbps, ' Mb/s', 0) : undefined,
        driver: iface.details?.driver,
    }, {
        icon: 'network',
        subtitle: label(iface.name === 'lo' ? 'local_interface' : 'network_interface'),
        state,
    });
    item.append(
        meter(label('receiving_now'), iface.rx_pct,
            finite(iface.rx_mbps) ? number(iface.rx_mbps, ' Mb/s') : label('waiting_for_rate')),
        meter(label('sending_now'), iface.tx_pct,
            finite(iface.tx_mbps) ? number(iface.tx_mbps, ' Mb/s') : label('waiting_for_rate')),
    );
    appendExpandable(item, `traffic-${index}`, label('traffic_totals_and_errors'), {
        total_received: iface.rx_bytes === undefined ? undefined : bytes(iface.rx_bytes),
        total_sent: iface.tx_bytes === undefined ? undefined : bytes(iface.tx_bytes),
        receive_errors: iface.rx_errors,
        send_errors: iface.tx_errors,
        receive_drops: iface.rx_dropped,
        send_drops: iface.tx_dropped,
    });
    appendExpandable(item, `interface-${index}`, label('interface_details'),
        without(iface.details, ['state', 'driver']));
    return item;
}

function network(data, grid) {
    const all = data.network || [];
    const activeInterfaces = all.filter(iface => iface.name !== 'lo' && iface.details?.state === 'up');
    const prominent = activeInterfaces.length ? activeInterfaces :
        all.filter(iface => iface.name !== 'lo').slice(0, 1);
    for (const iface of prominent) grid.append(interfaceCard(iface, all.indexOf(iface)));
    if (!all.length) grid.append(callout(label('no_interfaces')));

    const others = all.filter(iface => !prominent.includes(iface));
    if (others.length) {
        const item = card(label('other_network_interfaces'), null, {
            wide: true,
            icon: 'network',
            subtitle: label('other_interfaces_intro'),
        });
        item.append(expandable('other-interfaces', `${label('view_interfaces')} · ${others.length}`,
            table(['name', 'state', 'addresses', 'link_speed', 'driver'], others.map(iface => ({
                name: iface.name,
                state: translatedState(iface.details?.state),
                addresses: iface.addresses?.join(', '),
                link_speed: finite(iface.speed_mbps) ? number(iface.speed_mbps, ' Mb/s', 0) : undefined,
                driver: iface.details?.driver,
            })))));
        grid.append(item);
    }
    grid.append(callout(label('network_note')));
}

function devices(data, grid) {
    for (const [index, gpu] of (data.live?.gpu || []).entries()) {
        const item = card(gpu.name || `${label('graphics_device')} ${index + 1}`, {
            driver: gpu.driver,
            temperature: finite(gpu.temp) ? number(gpu.temp, ' °C') : undefined,
            power: finite(gpu.power_w) ? number(gpu.power_w, ' W') : undefined,
        }, { icon: 'gpu', subtitle: label('graphics_device') });
        item.append(meter(label('gpu_usage'), gpu.load_pct),
            meter(label('video_memory'), gpu.vram_pct,
                finite(gpu.vram_total) ? `${bytes(gpu.vram_used)} / ${bytes(gpu.vram_total)}` : '—'));
        grid.append(item);
    }

    for (const [index, usb] of (data.usb || []).entries()) {
        const primary = ['manufacturer', 'speed_mbps', 'max_power'];
        const item = card(usb.product || `${label('usb_device')} ${index + 1}`, select(usb, primary), {
            icon: 'usb',
            subtitle: usb.address ? `${label('port')} ${usb.address}` : label('usb_device'),
        });
        appendExpandable(item, `usb-${index}`, label('technical_details'),
            without(usb, [...primary, 'product', 'address']));
        grid.append(item);
    }

    if (data.pci?.length) {
        const item = card(label('internal_pci_devices'), null, {
            wide: true,
            icon: 'pci',
            subtitle: label('pci_devices_intro'),
        });
        const columns = [...new Set(data.pci.flatMap(row => Object.keys(row)))];
        item.append(expandable('pci-devices', `${label('view_devices')} · ${data.pci.length}`,
            table(columns, data.pci)));
        grid.append(item);
    }
    if (!(data.live?.gpu?.length || data.usb?.length || data.pci?.length)) {
        grid.append(callout(label('no_connected_devices')));
    }
}

function temperatureReading(sensor) {
    const value = Number(sensor.value);
    const minimum = 0;
    const maximum = 120;
    const bounded = Math.max(minimum, Math.min(maximum, value));
    const level = ((bounded - minimum) / (maximum - minimum)) * 100;
    const tone = value >= 90 ? ' is-critical' : value >= 75 ? ' is-hot' : value >= 55 ? ' is-warm' : '';
    const formatted = number(value, ` ${sensor.unit}`);
    const reading = node('div', 'system-info-temperature-reading');
    const gauge = node('div', `system-info-thermometer${tone}`);
    gauge.setAttribute('role', 'meter');
    gauge.setAttribute('aria-label', sensor.name);
    gauge.setAttribute('aria-valuemin', String(minimum));
    gauge.setAttribute('aria-valuemax', String(maximum));
    gauge.setAttribute('aria-valuenow', String(value));
    gauge.setAttribute('aria-valuetext', formatted);
    const track = node('span', 'system-info-thermometer-track');
    const fill = node('span', 'system-info-thermometer-fill');
    fill.style.setProperty('--temperature-level', `${level}%`);
    track.append(fill);
    gauge.append(track, node('span', 'system-info-thermometer-bulb'));
    reading.append(gauge, node('span', '', sensor.name), node('strong', '', formatted));
    return reading;
}

function sensors(data, grid) {
    const grouped = new Map();
    for (const sensor of data.sensors || []) {
        const list = grouped.get(sensor.device) || [];
        list.push(sensor);
        grouped.set(sensor.device, list);
    }
    for (const [device, sensorGroup] of grouped) {
        const item = card(humanize(device), null, { icon: 'sensor', subtitle: label('sensor_group') });
        const list = node('div', 'system-info-sensor-list');
        for (const sensor of sensorGroup) {
            const sensorUnit = String(sensor.unit || '').replaceAll(' ', '').toLowerCase();
            if (finite(sensor.value) && ['°c', 'c'].includes(sensorUnit)) {
                list.append(temperatureReading(sensor));
                continue;
            }
            const reading = node('div', 'system-info-sensor-reading');
            reading.append(node('span', '', sensor.name),
                node('strong', '', number(sensor.value, ` ${sensor.unit}`)));
            list.append(reading);
        }
        item.append(list);
        grid.append(item);
    }

    for (const [index, power] of (data.power || []).entries()) {
        const primary = ['type', 'status', 'health', 'capacity_percent', 'manufacturer', 'model'];
        const item = card(power.name || `${label('power_supply')} ${index + 1}`, select(power, primary), {
            icon: 'power',
            subtitle: label('power_supply'),
            state: power.online,
        });
        const capacity = Number(power.capacity_percent);
        if (Number.isFinite(capacity)) item.append(meter(label('charge_level'), capacity));
        appendExpandable(item, `power-${index}`, label('energy_and_hardware_details'),
            without(power, [...primary, 'name', 'online']));
        grid.append(item);
    }
    if (!grouped.size && !data.power?.length) grid.append(callout(label('no_sensors_or_power')));
}

const renderers = { overview, cpu, memory, storage, network, devices, sensors };

function sectionHeader() {
    const header = node('header', 'system-info-section-header');
    const text = node('div');
    text.append(node('h3', '', label(active)), node('p', '', label(`${active}_intro`)));
    header.append(text);
    return header;
}

function render() {
    if (!snapshot) return;
    const content = el('system-info-content');
    const opened = [...content.querySelectorAll('details[open]')].map(item => item.dataset.key);
    const focused = document.activeElement?.closest('details')?.dataset.key;
    const section = node('div', 'system-info-section');
    const grid = node('div', 'system-info-grid');
    renderers[active](snapshot, grid);
    if (!grid.children.length) {
        grid.append(node('p', 'system-info-empty system-info-wide', label('unavailable')));
    }
    section.append(sectionHeader(), grid);
    content.replaceChildren(section);
    content.setAttribute('aria-labelledby', `system-info-tab-${active}`);
    content.setAttribute('aria-busy', 'false');
    for (const item of content.querySelectorAll('details')) {
        item.open = opened.includes(item.dataset.key);
        if (item.dataset.key === focused) item.querySelector('summary').focus({ preventScroll: true });
    }
}

function renderLoading() {
    const content = el('system-info-content');
    const wrap = node('div', 'system-info-loading-grid');
    for (let index = 0; index < 6; index++) wrap.append(node('div', 'system-info-skeleton'));
    content.replaceChildren(wrap);
    content.setAttribute('aria-busy', 'true');
}

function renderError() {
    const content = el('system-info-content');
    const empty = node('div', 'system-info-empty');
    empty.append(node('p', '', label('load_failed')));
    content.replaceChildren(empty);
    content.setAttribute('aria-busy', 'false');
}

function activateSection(section, { focus = false } = {}) {
    if (!renderers[section]) return;
    active = section;
    for (const tab of el('system-info-tabs').children) {
        const selected = tab.dataset.section === active;
        tab.setAttribute('aria-selected', String(selected));
        tab.tabIndex = selected ? 0 : -1;
        if (selected && focus) tab.focus();
    }
    if (snapshot) render();
}

function tabs() {
    const nav = el('system-info-tabs');
    nav.replaceChildren();
    for (const section of sectionDefinitions) {
        const button = node('button', 'system-info-tab');
        button.type = 'button';
        button.id = `system-info-tab-${section.id}`;
        button.dataset.section = section.id;
        button.setAttribute('role', 'tab');
        button.setAttribute('aria-controls', 'system-info-content');
        button.setAttribute('aria-selected', String(section.id === active));
        button.tabIndex = section.id === active ? 0 : -1;
        button.append(node('span', 'system-info-tab-index', section.index),
            node('span', '', label(section.id)));
        button.addEventListener('click', () => activateSection(section.id));
        button.addEventListener('keydown', event => {
            const index = sectionDefinitions.findIndex(item => item.id === active);
            let next = index;
            if (event.key === 'ArrowRight') next = (index + 1) % sectionDefinitions.length;
            else if (event.key === 'ArrowLeft') next = (index - 1 + sectionDefinitions.length) % sectionDefinitions.length;
            else if (event.key === 'Home') next = 0;
            else if (event.key === 'End') next = sectionDefinitions.length - 1;
            else return;
            event.preventDefault();
            activateSection(sectionDefinitions[next].id, { focus: true });
        });
        nav.append(button);
    }
}

function pageIsOpen() {
    return !el('system-info-page')?.classList.contains('hidden');
}

function routeURL(hash = '') {
    return `${window.location.pathname}${window.location.search}${hash}`;
}

function routeState(open) {
    const state = { ...(window.history.state || {}) };
    if (open) state.kulaPage = 'system-info';
    else delete state.kulaPage;
    return Object.keys(state).length ? state : null;
}

function setStatus(text, tone = '') {
    const status = el('system-info-status');
    status.textContent = text;
    status.className = `system-info-status${tone ? ` is-${tone}` : ''}`;
}

function snapshotStatus() {
    if (!snapshot) return;
    const updated = new Date(snapshot.ts);
    let status = `${label('updated')} ${Number.isNaN(updated.getTime()) ? '—' :
        updated.toLocaleTimeString(i18n.currentLang)}`;
    if (snapshot.metrics_ts) {
        const metrics = new Date(snapshot.metrics_ts);
        status += ` · ${label('metrics_updated')} ${Number.isNaN(metrics.getTime()) ? '—' :
            metrics.toLocaleTimeString(i18n.currentLang)}`;
    } else {
        status += ` · ${label('metrics_pending')}`;
    }
    setStatus(status, 'live');
}

function stop() {
    clearTimeout(timer);
    timer = null;
    request?.abort();
    request = null;
    if (el('system-info-refresh')) el('system-info-refresh').disabled = false;
}

function hideSystemInfo({ clear = true, restoreScroll = true } = {}) {
    const wasOpen = pageIsOpen();
    stop();
    el('system-info-page')?.classList.add('hidden');
    el('system-info-page')?.setAttribute('aria-hidden', 'true');
    el('dashboard-home')?.classList.remove('hidden');
    el('dashboard-home')?.removeAttribute('aria-hidden');
    el('dashboard')?.classList.remove('system-info-active');
    el('btn-info')?.classList.remove('system-info-current');
    el('btn-info')?.removeAttribute('aria-current');
    if (clear) {
        snapshot = null;
        el('system-info-content')?.replaceChildren();
        setStatus('');
    }
    if (wasOpen && restoreScroll) {
        requestAnimationFrame(() => {
            window.scrollTo({ top: dashboardScroll, behavior: 'instant' });
            window.dispatchEvent(new Event('resize'));
        });
    }
}

export function closeSystemInfo({ useHistory = true } = {}) {
    const markedRoute = window.location.hash === routeHash &&
        window.history.state?.kulaPage === 'system-info';
    hideSystemInfo();
    if (window.location.hash !== routeHash) return;
    if (useHistory && markedRoute) window.history.back();
    else window.history.replaceState(routeState(false), '', routeURL());
}

export function showSystemInfo({ pushRoute = true, focusTitle = true } = {}) {
    if (!enabled) return;
    const firstOpen = !pageIsOpen();
    if (firstOpen) dashboardScroll = window.scrollY;
    el('dashboard-home')?.classList.add('hidden');
    el('dashboard-home')?.setAttribute('aria-hidden', 'true');
    el('system-info-page')?.classList.remove('hidden');
    el('system-info-page')?.removeAttribute('aria-hidden');
    el('dashboard')?.classList.add('system-info-active');
    el('btn-info')?.classList.add('system-info-current');
    el('btn-info')?.setAttribute('aria-current', 'page');
    if (pushRoute && window.location.hash !== routeHash) {
        window.history.pushState(routeState(true), '', routeURL(routeHash));
    }
    tabs();
    if (!snapshot) renderLoading();
    window.scrollTo({ top: 0, behavior: 'instant' });
    if (focusTitle) {
        requestAnimationFrame(() => el('system-info-title')?.focus({ preventScroll: true }));
    }
    refresh();
}

async function refresh() {
    if (!enabled || !pageIsOpen() || document.hidden || request) return;
    clearTimeout(timer);
    const controller = new AbortController();
    request = controller;
    const timeout = setTimeout(() => controller.abort(), 15000);
    el('system-info-refresh').disabled = true;
    setStatus(label(snapshot ? 'refreshing' : 'loading'), 'loading');
    try {
        const response = await fetch(apiUrl('/api/system-info'), {
            cache: 'no-store',
            signal: controller.signal,
        });
        if (controller !== request) return;
        if ([401, 403, 404].includes(response.status)) {
            closeSystemInfo({ useHistory: false });
            return;
        }
        if (!response.ok) throw new Error('Request failed');
        const data = await response.json();
        if (controller !== request) return;
        snapshot = data;
        snapshotStatus();
        render();
    } catch (_error) {
        if (controller === request) {
            setStatus(label(snapshot ? 'refresh_failed' : 'load_failed'), 'error');
            if (!snapshot) renderError();
        }
    } finally {
        clearTimeout(timeout);
        if (controller === request) {
            request = null;
            el('system-info-refresh').disabled = false;
            if (pageIsOpen() && !document.hidden) timer = setTimeout(refresh, 5000);
        }
    }
}

export function initSystemInfo() {
    tabs();
    el('btn-info').addEventListener('click', () => showSystemInfo());
    el('system-info-back').addEventListener('click', () => closeSystemInfo());
    el('system-info-refresh').addEventListener('click', refresh);
    window.addEventListener('popstate', () => {
        if (window.location.hash === routeHash && enabled) {
            showSystemInfo({ pushRoute: false, focusTitle: false });
        } else {
            hideSystemInfo();
        }
    });
    document.addEventListener('visibilitychange', () => {
        if (document.hidden) stop();
        else refresh();
    });
    document.addEventListener('kula-config-ready', event => {
        enabled = event.detail.show_system_info !== false;
        el('btn-info').classList.toggle('hidden', !enabled);
        if (!enabled) closeSystemInfo({ useHistory: false });
        else if (window.location.hash === routeHash && !pageIsOpen()) {
            showSystemInfo({ pushRoute: false });
        }
    });
    document.addEventListener('kula-i18n-changed', () => {
        tabs();
        if (snapshot) {
            render();
            snapshotStatus();
        }
    });
}
