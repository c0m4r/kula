/* Current hardware inventory has its own request lifecycle. Chart history,
   pause state, and WebSocket buffers never supply values to this page.
   Inventory and measured values are kept apart on purpose: a poll that only
   changes a temperature or a byte count patches the few live widgets in place
   instead of rebuilding the page, so scroll position, text selection, hover
   state and the mount search survive every refresh. */
import { apiUrl } from './api.js';
import { i18n } from './i18n.js';
import { formatBytesShort } from './format.js';

const sectionDefinitions = [
    { id: 'overview', icon: 'server' },
    { id: 'storage', icon: 'storage' },
    { id: 'network', icon: 'network' },
    { id: 'devices', icon: 'devices' },
    { id: 'sensors', icon: 'sensor' },
];
const sectionIDs = new Set(sectionDefinitions.map(section => section.id));
const routeHash = '#system-info';
const pollInterval = 5000;
let active = 'overview';
let snapshot = null;
let timer = null;
let request = null;
let enabled = true;
let dashboardScroll = 0;
const expandedSections = new Map();
let renderedSection = null;
let storageQuery = '';
let deviceQuery = '';
let structure = '';
let confirmation = null;

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
    return words.replace(/\b(cpu|gpu|usb|pci|uuid|wwid|mtu|iops|io|ecc|bios|numa|rpm|mhz|mbps|uefi|ip|hdd|ssd|nvme|acpi|tcp|mac)\b/gi,
        word => word.toUpperCase());
}

const label = key => i18n.translations[`si_${normalizedKey(key)}`] ||
    i18n.translations[normalizedKey(key)] || humanize(key);
// Free-form identifiers (kernel sensor names, PCI classes) are never mangled
// into a fake label: the raw value is shown when nothing is translated.
const rawLabel = (key, fallback) => i18n.translations[`si_${normalizedKey(key)}`] ||
    i18n.translations[normalizedKey(key)] || fallback;
const number = (value, unit = '', digits = 1) => present(value) && finite(Number(value))
    ? `${Number(value).toLocaleString(i18n.currentLang, { maximumFractionDigits: digits })}${unit}` : '—';
const bytes = value => present(value) && finite(Number(value)) ? formatBytesShort(Number(value)) : '—';
const percent = value => present(value) && finite(Number(value)) ? number(value, '%', 0) : '—';

function node(tag, className, text) {
    const element = document.createElement(tag);
    if (className) element.className = className;
    if (text !== undefined) element.textContent = String(text);
    return element;
}

function code(text, className = 'system-info-path') {
    const element = node('code', className, text);
    return element;
}

const iconPaths = {
    activity: ['M3 12h4l2.5-6 5 12 2.5-6h4'],
    bolt: ['M13 2 4 14h6l-1 8 9-12h-6l1-8Z'],
    chevron: ['M9 5l7 7-7 7'],
    cpu: ['M9 2v2M15 2v2M9 20v2M15 20v2M20 9h2M20 15h2M2 9h2M2 15h2',
        'M6 4h12a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2Z', 'M9 9h6v6H9Z'],
    copy: ['M9 9h9a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H9a2 2 0 0 1-2-2v-9a2 2 0 0 1 2-2Z',
        'M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1'],
    devices: ['M4 5h16v11H4Z', 'M8 20h8M12 16v4'],
    droplet: ['M12 3s6 6.5 6 11a6 6 0 0 1-12 0c0-4.5 6-11 6-11Z'],
    fan: ['M12 12a2 2 0 1 0 0 .01', 'M12 10c0-3-1-7-4-7S5 6 8 9c1 .8 2.5 1 4 1Z',
        'M14 12c3 0 7-1 7-4s-3-3-6 0c-.8 1-1 2.5-1 4Z', 'M12 14c0 3 1 7 4 7s3-3 0-6c-1-.8-2.5-1-4-1Z',
        'M10 12c-3 0-7 1-7 4s3 3 6 0c.8-1 1-2.5 1-4Z'],
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
    return ['up', 'online', '1', 'connected', 'full', 'charging'].includes(String(value ?? '').toLowerCase());
}

function isOffline(value) {
    return ['down', 'offline', '0', 'disconnected'].includes(String(value ?? '').toLowerCase());
}

function translatedState(value) {
    const state = String(value ?? '').toLowerCase();
    if (isOnline(state)) return label('online');
    if (isOffline(state)) return label('offline');
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
    if (name === 'used_pct' && Number.isFinite(numeric)) return percent(numeric);
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

function details(values = {}) {
    const list = node('dl', 'system-info-facts');
    for (const [key, value] of Object.entries(values || {})) {
        if (!present(value) || (typeof value === 'object' && !Array.isArray(value))) continue;
        list.append(node('dt', '', label(key)), node('dd', '', displayValue(key, value)));
    }
    if (!list.children.length) return node('p', 'system-info-empty', label('unavailable'));
    return list;
}

function heading(level, title, iconName, subtitle) {
    const head = node('header', 'system-info-card-heading');
    if (iconName) head.append(icon(iconName, 'system-info-card-icon'));
    const text = node('div');
    text.append(node(level, '', title));
    if (subtitle) text.append(node('p', '', subtitle));
    head.append(text);
    return head;
}

function stateChip(state) {
    return node('span', `system-info-state ${isOnline(state) ? 'is-online' : isOffline(state) ? 'is-offline' : ''}`,
        translatedState(state));
}

function trackedChip(tracked) {
    const chip = node('span', `system-info-chip is-tracking ${tracked ? 'is-tracked' : 'is-untracked'}`,
        label(tracked ? 'tracked' : 'not_tracked'));
    chip.title = label(tracked ? 'tracked_hint' : 'not_tracked_hint');
    return chip;
}

function card(title, values, options = {}) {
    const item = node('section', `system-info-card${options.wide ? ' system-info-wide' : ''}`);
    const head = heading('h3', title, options.icon, options.subtitle);
    if (present(options.state)) head.append(stateChip(options.state));
    item.append(head);
    if (values) item.append(details(values));
    return item;
}

function expandable(key, title, child) {
    const wrap = node('details', 'system-info-expandable');
    wrap.dataset.key = key;
    const summary = node('summary');
    if (title instanceof Node) summary.append(title);
    else summary.textContent = String(title);
    wrap.append(summary, child);
    return wrap;
}

function callout(text, hint) {
    const wrap = node('div', 'system-info-note system-info-wide');
    wrap.append(node('p', '', text));
    if (hint) wrap.append(node('p', 'system-info-note-hint', hint));
    return wrap;
}

function liveBox(kind, key, className = 'system-info-live') {
    const box = node('div', className);
    box.dataset.live = kind;
    if (key) box.dataset.key = key;
    return box;
}

/* ---------------------------------------------------------------- overview */

function summaryCard(options) {
    const linked = Boolean(options.section);
    const item = node(linked ? 'button' : 'article', `system-info-summary-card${linked ? ' is-link' : ''}`);
    if (linked) {
        item.type = 'button';
        item.dataset.summarySection = options.section;
        item.addEventListener('click', () => activateSection(options.section, { focus: true }));
        item.title = label('open_section');
    }
    const top = node('div', 'system-info-summary-top');
    top.append(icon(options.icon, 'system-info-summary-icon'), node('span', 'system-info-summary-title', options.title));
    if (linked) top.append(icon('chevron', 'system-info-summary-chevron'));
    const value = node('strong', 'system-info-summary-value', options.value);
    // A value derived from the latest sample must be patchable on its own, or a
    // poll that only moves that number would leave the card showing a stale one.
    if (options.liveValue) value.dataset.live = options.liveValue;
    item.append(top, value, node('span', 'system-info-summary-detail', options.detail));
    if (options.live) item.append(options.live);
    return item;
}

function profile(data) {
    const system = data.system || {};
    const item = node('section', 'system-info-profile system-info-wide');
    const primary = node('div');
    primary.append(node('h3', '', system.hostname || label('unnamed_system')));
    const identity = [system.manufacturer, system.family].filter(present).join(' · ');
    const description = [identity, system.product].filter(present).join(' · ') ||
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
    for (const [name, value] of [
        [label('kernel'), system.kernel],
        [label('uptime'), data.live?.sys?.uptime_human],
    ]) {
        const fact = node('div', 'system-info-profile-fact');
        const strong = node('strong', '', value || '—');
        if (present(value)) {
            strong.classList.add('system-info-copyable');
            strong.dataset.copy = String(value);
            strong.title = label('copy_value');
        }
        fact.append(node('span', '', name), strong);
        side.append(fact);
    }
    item.append(primary, side);
    return item;
}

function mainFilesystem(data) {
    return data.filesystems?.find(filesystem => filesystem.mount === '/' && filesystem.usage) ||
        data.filesystems?.find(filesystem => filesystem.usage);
}

function primaryInterface(data) {
    return data.network?.find(iface => iface.kind === 'wired' && iface.details?.state === 'up') ||
        data.network?.find(iface => iface.name !== 'lo' && iface.details?.state === 'up') ||
        data.network?.find(iface => iface.name !== 'lo') || data.network?.[0];
}

function overview(data, grid) {
    grid.append(profile(data));
    const cpu = data.cpu || {};
    const coreCount = cpu.cores || cpu.logical_cpus;
    const summaries = node('section', 'system-info-summary-grid system-info-wide');
    summaries.append(
        summaryCard({
            icon: 'cpu', title: label('processor'), value: cpu.model_name || label('unavailable'),
            detail: [
                coreCount ? `${number(coreCount, '', 0)} ${label('cores')}` : '',
                cpu.logical_cpus ? `${number(cpu.logical_cpus, '', 0)} ${label('logical_cpus')}` : '',
            ].filter(Boolean).join(' · '),
            live: liveBox('cpu'),
        }),
        summaryCard({
            icon: 'memory', title: label('ram'), value: bytes(data.live?.mem?.total),
            detail: label('installed_memory'), liveValue: 'mem-total', live: liveBox('mem'),
        }),
        summaryCard({
            icon: 'storage', title: label('main_storage'),
            value: bytes(mainFilesystem(data)?.usage?.total),
            detail: mainFilesystem(data)?.mount || label('storage_usage_unavailable'),
            section: 'storage', live: liveBox('fs', mainFilesystem(data)?.mount),
        }),
        summaryCard({
            icon: 'network', title: label('network'), value: primaryInterface(data)?.name || label('unavailable'),
            detail: primaryInterface(data)?.addresses?.[0] || label('no_interfaces'),
            section: 'network',
        }),
    );
    grid.append(summaries);
}

/* ----------------------------------------------------------------- storage */

// A mount path is one value, even when it contains spaces or commas.
function mountPaths(mounts, key) {
    const paths = [...new Set(mounts.filter(present))].sort(compareMounts);
    if (!paths.length) return node('span', '', '—');
    const wrap = node('div', 'system-info-mount-paths');
    const list = values => {
        const items = node('ul', 'system-info-path-list');
        for (const path of values) {
            const item = node('li');
            item.append(code(path));
            items.append(item);
        }
        return items;
    };
    wrap.append(list(paths.slice(0, 3)));
    if (paths.length > 3) {
        wrap.append(expandable(key, `${label('more_mounts')} · ${number(paths.length - 3, '', 0)}`,
            list(paths.slice(3))));
    }
    return wrap;
}

function compareMounts(a, b) {
    if (a === b) return 0;
    if (a === '/') return -1;
    if (b === '/') return 1;
    return a.localeCompare(b, i18n.currentLang, { numeric: true });
}

function runtimeMount(filesystem) {
    // Keep the root filesystem visible even in a container or live system.
    return filesystem.mount !== '/' &&
        ['tmpfs', 'devtmpfs', 'squashfs', 'overlay', 'nsfs', 'ramfs'].includes(filesystem.type);
}

function filesystemList(filesystems) {
    const list = node('div', 'system-info-filesystems');
    for (const filesystem of filesystems) {
        const item = node('article', 'system-info-filesystem');
        const identity = node('div', 'system-info-filesystem-identity');
        const head = node('div', 'system-info-filesystem-head');
        const mount = code(filesystem.mount || label('unknown_mount'));
        mount.dataset.copy = filesystem.mount || '';
        mount.title = label('copy_value');
        head.append(mount);
        if (present(filesystem.type)) head.append(node('span', 'system-info-fs-type', filesystem.type));
        if (!filesystem.tracked) head.append(trackedChip(false));
        const meta = node('div', 'system-info-filesystem-meta');
        if (present(filesystem.device)) meta.append(code(filesystem.device, 'system-info-device'));
        identity.append(head, meta);
        item.append(identity, liveBox('fs', filesystem.mount, 'system-info-filesystem-usage'));
        list.append(item);
    }
    return list;
}

function mountedStorage(filesystems) {
    const item = card(label('mounted_storage'), null, {
        wide: true, icon: 'filesystem', subtitle: label('mounted_storage_intro'),
    });
    item.classList.add('system-info-mounted-storage');
    const toolbar = node('div', 'system-info-mount-toolbar');
    const search = node('label', 'system-info-mount-search');
    search.append(node('span', 'sr-only', label('search_mounts')));
    const input = node('input');
    input.type = 'search';
    input.id = 'system-info-mount-search';
    input.placeholder = label('search_mounts');
    input.autocomplete = 'off';
    input.spellcheck = false;
    input.value = storageQuery;
    search.append(input);
    const count = node('span', 'system-info-mount-count');
    count.setAttribute('role', 'status');
    toolbar.append(search, count);
    const results = node('div');
    const sorted = [...filesystems].sort((a, b) => compareMounts(a.mount || '', b.mount || ''));
    const update = () => {
        const opened = [...results.querySelectorAll('details[open]')].map(details => details.dataset.key);
        const query = storageQuery.trim().toLocaleLowerCase(i18n.currentLang);
        const matches = sorted.filter(fs => !query || [fs.mount, fs.device, fs.type]
            .some(value => String(value || '').toLocaleLowerCase(i18n.currentLang).includes(query)));
        count.textContent = `${query ? `${number(matches.length, '', 0)} / ` : ''}${number(sorted.length, '', 0)} ${label('mounts').toLowerCase()}`;
        results.replaceChildren();
        const primary = matches.filter(fs => !runtimeMount(fs));
        const runtime = matches.filter(runtimeMount);
        if (primary.length) results.append(filesystemList(primary));
        if (runtime.length) {
            const group = expandable('runtime-mounts',
                `${label('system_runtime_mounts')} · ${number(runtime.length, '', 0)}`, filesystemList(runtime));
            group.classList.add('system-info-runtime-mounts');
            results.append(group);
        }
        if (!matches.length) results.append(node('p', 'system-info-empty', label('no_matching_mounts')));
        for (const details of results.querySelectorAll('details')) {
            details.open = opened.includes(details.dataset.key) || Boolean(query && details.dataset.key === 'runtime-mounts');
        }
        patchValues(snapshot);
    };
    input.addEventListener('input', () => {
        storageQuery = input.value;
        update();
    });
    update();
    item.append(toolbar, results);
    return item;
}

function driveMounts(disk) {
    if (!disk.mounts?.length) return null;
    const mounts = node('div', 'system-info-drive-mounts');
    mounts.append(node('span', 'system-info-mount-label', label('mounted_at')),
        mountPaths(disk.mounts, `drive-mounts-${disk.name}`));
    return mounts;
}

function driveCard(disk) {
    const info = disk.details || {};
    const title = info.model || info.volume_name || `/dev/${disk.name}`;
    const item = card(title, {
        capacity: disk.size_bytes === undefined ? undefined : bytes(disk.size_bytes),
        drive_type: [rawLabel(`medium_${disk.medium}`, ''), rawLabel(`class_${disk.class}`, '')]
            .filter(Boolean).join(' · ') || undefined,
    }, { icon: 'storage', subtitle: `/dev/${disk.name}` });
    item.classList.add('system-info-drive');
    item.querySelector('.system-info-card-heading')?.append(trackedChip(disk.tracked));
    const mounts = driveMounts(disk);
    if (mounts) item.append(mounts);
    return item;
}

function storage(data, grid) {
    if (data.filesystems?.length) grid.append(mountedStorage(data.filesystems));

    const all = data.disks || [];
    const drives = all.filter(disk => disk.class === 'disk' && !disk.name.startsWith('loop'));
    const stacked = all.filter(disk => !(disk.class === 'disk' && !disk.name.startsWith('loop')));
    for (const disk of drives) grid.append(driveCard(disk));
    if (stacked.length) {
        const body = node('div', 'system-info-grid');
        for (const disk of stacked) body.append(driveCard(disk));
        const group = expandable('stacked-devices',
            `${label('virtual_devices')} · ${number(stacked.length, '', 0)}`, body);
        group.classList.add('system-info-wide', 'system-info-stacked');
        grid.append(group);
    }
    if (!all.length) grid.append(callout(label('no_storage_devices')));
}

/* ----------------------------------------------------------------- network */

function interfaceKindLabel(iface) {
    if (iface.kind === 'local') return label('local_interface');
    if (iface.kind === 'wireless') return label('wireless_interface');
    if (iface.kind === 'virtual') return label('virtual_interface');
    if (iface.kind === 'wired') return label('wired_interface');
    // An unrecognised kind is described generically rather than guessed at.
    return label('network_interface');
}

function interfaceCard(iface) {
    const item = card(iface.name, {
        ip_addresses: iface.addresses?.join('\n'),
        mac_address: iface.mac,
        mtu: iface.mtu,
        link_speed: finite(iface.speed_mbps) ? number(iface.speed_mbps, ' Mb/s', 0) : undefined,
        driver: iface.details?.driver,
    }, {
        icon: iface.kind === 'wireless' ? 'network' : 'devices',
        subtitle: interfaceKindLabel(iface),
        state: iface.details?.state,
    });
    item.classList.add('system-info-interface');
    item.querySelector('.system-info-card-heading')?.append(trackedChip(iface.tracked));
    return item;
}

const interfaceOrder = { wired: 0, wireless: 1, virtual: 2, local: 3 };

function network(data, grid) {
    const all = [...(data.network || [])].sort((a, b) =>
        (interfaceOrder[a.kind] ?? 4) - (interfaceOrder[b.kind] ?? 4) ||
        String(a.name).localeCompare(String(b.name), i18n.currentLang, { numeric: true }));
    for (const iface of all) grid.append(interfaceCard(iface));
    if (!all.length) grid.append(callout(label('no_interfaces')));
}

/* ----------------------------------------------------------------- devices */

function inventoryList() {
    return node('ul', 'system-info-inventory-list');
}

function inventoryItem(iconName, title, subtitle, values, state, extra) {
    const item = node('li', 'system-info-inventory-item');
    if (extra) item.classList.add(extra);
    item.append(icon(iconName, 'system-info-card-icon'));
    const body = node('div', 'system-info-inventory-body');
    const head = node('div', 'system-info-inventory-heading');
    const titleWrap = node('div');
    titleWrap.append(node('h4', '', title));
    if (subtitle) titleWrap.append(node('p', '', subtitle));
    head.append(titleWrap);
    if (present(state)) head.append(stateChip(state));
    body.append(head);
    if (values && Object.values(values).some(present)) body.append(details(values));
    item.append(body);
    return item;
}

const classOrder = ['display', 'network', 'storage', 'usb', 'multimedia', 'communication', 'wireless',
    'encryption', 'memory', 'input', 'video', 'audio', 'hub', 'printer', 'processor', 'system', 'other'];

const classIcons = {
    display: 'gpu', network: 'network', storage: 'storage', usb: 'usb', multimedia: 'activity',
    communication: 'network', wireless: 'network', encryption: 'hardware', memory: 'memory',
    input: 'devices', video: 'devices', audio: 'activity', hub: 'usb', printer: 'devices',
    processor: 'cpu', system: 'hardware', other: 'pci',
};

const classLabel = slug => rawLabel(`class_${slug}`, humanize(slug || 'other'));

function deviceGroups(data) {
    const groups = new Map();
    const push = (slug, item) => {
        const key = classOrder.includes(slug) ? slug : 'other';
        if (!groups.has(key)) groups.set(key, []);
        groups.get(key).push(item);
    };
    for (const [index, gpu] of (data.live?.gpu || []).entries()) {
        push('display', inventoryItem('gpu', gpu.name || `${label('graphics_device')} ${index + 1}`,
            label('graphics_device'), { driver: gpu.driver }));
    }
    for (const usb of data.usb || []) {
        const title = usb.product || usb.manufacturer || `${label('usb_device')} ${usb.address || ''}`.trim();
        push(usb.class || 'other', inventoryItem(classIcons[usb.class] || 'usb', title,
            classLabel(usb.class), {
                manufacturer: usb.manufacturer,
                device_id: usb.device_id,
                speed_mbps: usb.speed_mbps,
                max_power: usb.max_power,
                port: usb.address,
            }));
    }
    for (const pci of data.pci || []) {
        push(pci.class || 'other', inventoryItem(classIcons[pci.class] || 'pci', classLabel(pci.class),
            pci.vendor || label('internal_pci_devices'), {
                driver: pci.driver,
                device_id: pci.device_id,
                bus_address: pci.address,
            }));
    }
    return [...groups.entries()].sort((a, b) => classOrder.indexOf(a[0]) - classOrder.indexOf(b[0]));
}

function devices(data, grid) {
    const groups = deviceGroups(data);
    const total = groups.reduce((sum, [, items]) => sum + items.length, 0);
    if (!groups.length || !total) {
        grid.append(callout(label('no_connected_devices'), label('no_devices_hint')));
        return;
    }
    const toolbar = node('div', 'system-info-mount-toolbar system-info-wide');
    const search = node('label', 'system-info-mount-search');
    search.append(node('span', 'sr-only', label('search_devices')));
    const input = node('input');
    input.type = 'search';
    input.id = 'system-info-device-search';
    input.placeholder = label('search_devices');
    input.autocomplete = 'off';
    input.spellcheck = false;
    input.value = deviceQuery;
    search.append(input);
    const count = node('span', 'system-info-mount-count');
    count.setAttribute('role', 'status');
    toolbar.append(search, count);
    grid.append(toolbar);

    const results = node('div', 'system-info-device-groups system-info-wide');
    const update = () => {
        const opened = [...results.querySelectorAll('details[open]')].map(item => item.dataset.key);
        const query = deviceQuery.trim().toLocaleLowerCase(i18n.currentLang);
        let shown = 0;
        results.replaceChildren();
        for (const [slug, items] of groups) {
            const matches = query ? items.filter(item =>
                item.textContent.toLocaleLowerCase(i18n.currentLang).includes(query)) : items;
            if (!matches.length) continue;
            shown += matches.length;
            const list = inventoryList();
            list.append(...matches);
            const body = node('div');
            body.append(list);
            const summary = node('h3', '', `${classLabel(slug)} · ${number(matches.length, '', 0)}`);
            const group = expandable(`device-class-${slug}`, summary, body);
            group.classList.add('system-info-device-group');
            group.open = opened.includes(group.dataset.key) || Boolean(query) || matches.length <= 6;
            results.append(group);
        }
        count.textContent = `${query ? `${number(shown, '', 0)} / ` : ''}${number(total, '', 0)} ${label('devices').toLowerCase()}`;
        if (!shown) results.append(node('p', 'system-info-empty', label('no_matching_devices')));
    };
    input.addEventListener('input', () => {
        deviceQuery = input.value;
        update();
    });
    update();
    grid.append(results);
}

/* ----------------------------------------------------------------- sensors */

const sensorKindIcons = {
    temperature: 'sensor', fan: 'fan', voltage: 'bolt', current: 'activity',
    power: 'power', humidity: 'droplet',
};

function sensorKind(sensor) {
    if (sensor.kind) return sensor.kind;
    // Older payloads carried no kind; the unit is unambiguous enough to recover it.
    switch (String(sensor.unit || '')) {
        case '°C': return 'temperature';
        case 'RPM': return 'fan';
        case 'V': return 'voltage';
        case 'A': return 'current';
        case 'W': return 'power';
        case '%': return 'humidity';
    }
    return '';
}

const sensorKindOrder = ['temperature', 'fan', 'power', 'voltage', 'current', 'humidity', ''];

function sensorKindLabel(sensor) {
    const kind = sensorKind(sensor);
    return kind ? label(`kind_${kind}`) : label('kind_sensor');
}

const deviceNames = {
    k10temp: 'CPU', coretemp: 'CPU', zenpower: 'CPU', cpu_thermal: 'CPU', k8temp: 'CPU',
    amdgpu: 'GPU', nouveau: 'GPU', radeon: 'GPU', i915: 'GPU', xe: 'GPU', nvidia: 'GPU',
    nvme: 'NVMe', acpitz: 'ACPI', acpi: 'ACPI',
    BAT0: 'Battery', BAT1: 'Battery', BAT2: 'Battery', ACAD: 'AC adapter', AC: 'AC adapter', ADP1: 'AC adapter',
    thinkpad: 'ThinkPad', asus: 'ASUS EC', dell_smm: 'Dell EC', dell_ddv: 'Dell EC', hp_wmi: 'HP EC',
    iwlwifi: 'Wireless', mt7921_phy0: 'Wireless', ath10k_hwmon: 'Wireless', amdgpu_hwmon: 'GPU',
};

function deviceLabel(device) {
    if (deviceNames[device]) return deviceNames[device];
    const zone = /^thermal_zone(\d+)$/.exec(device || '');
    if (zone) return `${label('thermal_zone')} ${zone[1]}`;
    return device || label('unknown_device');
}

function sensorValue(sensor) {
    const wrap = node('div', 'system-info-sensor-reading');
    wrap.append(node('strong', '', number(sensor.value, ` ${sensor.unit}`)));
    return wrap;
}

function temperatureSeverity(value) {
    if (!finite(Number(value))) return '';
    if (Number(value) >= 90) return 'is-critical';
    if (Number(value) >= 75) return 'is-hot';
    if (Number(value) >= 60) return 'is-warm';
    return '';
}

function sensorGroups(data) {
    const groups = new Map();
    for (const sensor of data.sensors || []) {
        const key = sensor.device || label('unknown_device');
        if (!groups.has(key)) groups.set(key, []);
        groups.get(key).push(sensor);
    }
    for (const sensors of groups.values()) {
        sensors.sort((a, b) => sensorKindOrder.indexOf(sensorKind(a)) - sensorKindOrder.indexOf(sensorKind(b)) ||
            String(a.name).localeCompare(String(b.name), i18n.currentLang, { numeric: true }));
    }
    return [...groups.entries()].sort((a, b) => {
        const rank = sensors => Math.min(...sensors.map(sensor => sensorKindOrder.indexOf(sensorKind(sensor))));
        return rank(a[1]) - rank(b[1]) || String(a[0]).localeCompare(String(b[0]), i18n.currentLang);
    });
}

function sensorItem(sensor) {
    const item = node('li', `system-info-inventory-item ${temperatureSeverity(sensor.value)}`.trim());
    item.append(icon(sensorKindIcons[sensorKind(sensor)] || 'sensor', 'system-info-card-icon'));
    const body = node('div', 'system-info-inventory-body');
    const head = node('div', 'system-info-inventory-heading');
    const titleWrap = node('div');
    titleWrap.append(node('h4', '', sensor.name || label('unknown_sensor')));
    titleWrap.append(node('p', '', sensorKindLabel(sensor)));
    head.append(titleWrap);
    head.append(liveBox('sensor', `${sensor.device}\u0001${sensor.name}`, 'system-info-sensor-live'));
    body.append(head);
    item.append(body);
    return item;
}

function sensors(data, grid) {
    const groups = sensorGroups(data);
    if (!groups.length && !(data.power || []).length) {
        grid.append(callout(label('no_sensors_or_power'), label('no_sensors_hint')));
        return;
    }
    for (const [device, sensors] of groups) {
        const list = inventoryList();
        for (const sensor of sensors) list.append(sensorItem(sensor));
        const body = node('div');
        body.append(list);
        const section = node('section', 'system-info-sensor-group system-info-wide');
        const head = node('header', 'system-info-group-heading');
        head.append(node('h3', '', deviceLabel(device)));
        if (deviceLabel(device) !== device) head.append(code(device, 'system-info-device-name'));
        section.append(head, body);
        grid.append(section);
    }
    if ((data.power || []).length) {
        const list = inventoryList();
        for (const [index, power] of (data.power || []).entries()) {
            const item = inventoryItem('power', power.name || `${label('power_supply')} ${index + 1}`,
                label('power_supply'), select(power, ['status', 'capacity_percent']), power.online);
            list.append(item);
        }
        const body = node('div');
        body.append(list);
        const section = node('section', 'system-info-sensor-group system-info-wide is-power');
        const head = node('header', 'system-info-group-heading');
        head.append(node('h3', '', label('power_supply')));
        section.append(head, body);
        grid.append(section);
    }
}

/* ------------------------------------------------------------------ render */

function renderSection(section, data, grid) {
    switch (section) {
    case 'overview': overview(data, grid); break;
    case 'storage': storage(data, grid); break;
    case 'network': network(data, grid); break;
    case 'devices': devices(data, grid); break;
    case 'sensors': sensors(data, grid); break;
    }
}

function render() {
    if (!snapshot) return;
    const content = el('system-info-content');
    const openKeys = [...content.querySelectorAll('details[open]')].map(item => item.dataset.key);
    if (renderedSection) expandedSections.set(renderedSection, openKeys);
    // A section that has never been rendered keeps whatever defaults its
    // renderer chose; a revisited one is restored exactly as the user left it.
    const known = expandedSections.has(active);
    const opened = expandedSections.get(active) || [];
    const focused = renderedSection === active && content.contains(document.activeElement)
        ? document.activeElement?.closest('details')?.dataset.key : null;
    const focusedSearchId = renderedSection === active &&
        ['system-info-mount-search', 'system-info-device-search'].includes(document.activeElement?.id)
        ? document.activeElement.id : null;
    const selection = focusedSearchId ? [document.activeElement.selectionStart, document.activeElement.selectionEnd] : null;
    const focusedSummary = renderedSection === active ? document.activeElement?.dataset.summarySection : null;
    renderedSection = active;
    const section = node('div', 'system-info-section');
    const grid = node('div', 'system-info-grid');
    renderSection(active, snapshot, grid);
    if (!grid.children.length) {
        grid.append(node('p', 'system-info-empty system-info-wide', label('unavailable')));
    }
    section.append(grid);
    content.replaceChildren(section);
    content.setAttribute('aria-labelledby', `system-info-tab-${active}`);
    content.setAttribute('aria-busy', 'false');
    patchValues(snapshot);
    for (const item of content.querySelectorAll('details')) {
        if (known) item.open = opened.includes(item.dataset.key);
        if (item.dataset.key === focused && !focusedSearchId) item.querySelector('summary').focus({ preventScroll: true });
    }
    if (focusedSearchId) {
        const input = el(focusedSearchId);
        input?.focus({ preventScroll: true });
        input?.setSelectionRange(...selection);
    }
    if (focusedSummary) {
        content.querySelector(`[data-summary-section="${focusedSummary}"]`)?.focus({ preventScroll: true });
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

/* ------------------------------------------------------------------- live */

function usageMeter(usage) {
    const wrap = node('div', 'system-info-meter');
    if (!usage) {
        wrap.classList.add('is-empty');
        wrap.append(node('span', 'system-info-empty-usage', label('no_storage_usage')));
        return wrap;
    }
    const pct = finite(Number(usage.used_pct)) ? Number(usage.used_pct)
        : usage.total ? (Number(usage.used) / Number(usage.total)) * 100 : NaN;
    const level = finite(pct) ? Math.min(100, Math.max(0, pct)) : null;
    const track = node('div', `system-info-progress${level === null ? '' :
        level >= 90 ? ' is-critical' : level >= 75 ? ' is-warning' : ''}`);
    track.setAttribute('role', 'progressbar');
    track.setAttribute('aria-valuemin', '0');
    track.setAttribute('aria-valuemax', '100');
    if (level !== null) {
        track.setAttribute('aria-valuenow', String(Math.round(level)));
        track.setAttribute('aria-label', label('usage'));
    }
    const fill = node('div', 'system-info-progress-fill');
    if (level !== null) fill.style.setProperty('--meter-value', `${level}%`);
    track.append(fill);
    const text = node('div', 'system-info-meter-label');
    text.append(node('strong', '', percent(level)),
        node('span', '', `${label('used')} ${bytes(usage.used)} · ${label('free')} ${bytes(usage.available)}`));
    wrap.append(track, text);
    return wrap;
}

function liveContent(kind, key, data) {
    const nodes = [];
    if (kind === 'mem-total') {
        nodes.push(document.createTextNode(bytes(data.live?.mem?.total)));
        return nodes;
    }
    if (kind === 'cpu') {
        const cpu = data.live?.cpu;
        nodes.push(node('span', 'system-info-summary-live',
            `${label('now')} ${percent(cpu?.usage_pct)} · ${label('load')} ${number(cpu?.load1, '', 2)}`));
        return nodes;
    }
    if (kind === 'mem') {
        const mem = data.live?.mem;
        nodes.push(usageMeter(mem?.total ? { total: mem.total, used: mem.used, available: mem.total - mem.used, used_pct: mem.used_pct } : null));
        return nodes;
    }
    if (kind === 'fs') {
        const filesystem = (data.filesystems || []).find(item => item.mount === key);
        nodes.push(usageMeter(filesystem?.usage));
        return nodes;
    }
    if (kind === 'sensor') {
        const [device, name] = String(key || '').split('\u0001');
        const sensor = (data.sensors || []).find(item => item.device === device && item.name === name);
        if (sensor) nodes.push(...sensorValue(sensor).childNodes);
        return nodes;
    }
    return nodes;
}

// patchValues refreshes every measured widget the current section shows without
// touching the surrounding inventory markup.
function patchValues(data) {
    if (!data) return;
    const content = el('system-info-content');
    if (!content) return;
    for (const box of content.querySelectorAll('[data-live]')) {
        const kind = box.dataset.live;
        const replacement = liveContent(kind, box.dataset.key, data);
        box.replaceChildren(...replacement);
        if (kind === 'sensor') {
            const [, name] = String(box.dataset.key || '').split('\u0001');
            const sensor = (data.sensors || []).find(item => item.name === name);
            for (const className of ['is-warm', 'is-hot', 'is-critical']) {
                box.closest('.system-info-inventory-item')?.classList.remove(className);
            }
            const severity = temperatureSeverity(sensor?.value);
            if (severity) box.closest('.system-info-inventory-item')?.classList.add(severity);
        }
    }
}

// structureKey covers identity and topology only. Temperatures, byte counts and
// percentages are excluded so that a steady machine does not rebuild the page
// on every poll.
function structureKey(data) {
    return JSON.stringify({
        system: data.system, cpu: data.cpu, disks: data.disks, network: data.network,
        pci: data.pci, usb: data.usb, power: data.power,
        filesystems: (data.filesystems || []).map(fs => ({
            device: fs.device, mount: fs.mount, type: fs.type, tracked: fs.tracked,
            total: fs.usage?.total,
        })),
        sensors: (data.sensors || []).map(sensor => ({
            device: sensor.device, name: sensor.name, unit: sensor.unit, kind: sensor.kind,
        })),
        gpu: data.live?.gpu,
    });
}

/* --------------------------------------------------------------- lifecycle */

function activateSection(section, { focus = false, updateRoute = true } = {}) {
    if (!sectionIDs.has(section)) return;
    active = section;
    el('system-info-content').setAttribute('aria-labelledby', `system-info-tab-${active}`);
    for (const tab of el('system-info-tabs').children) {
        const selected = tab.dataset.section === active;
        tab.setAttribute('aria-selected', String(selected));
        tab.tabIndex = selected ? 0 : -1;
        if (selected && focus) tab.focus();
        if (selected) {
            const bounds = tab.getBoundingClientRect();
            const viewport = tab.parentElement.getBoundingClientRect();
            // Reveal the tab horizontally without moving the page vertically.
            const offset = bounds.left < viewport.left ? bounds.left - viewport.left - 8
                : bounds.right > viewport.right ? bounds.right - viewport.right + 8 : 0;
            if (offset) tab.parentElement.scrollBy({ left: offset, behavior: 'instant' });
        }
    }
    if (updateRoute && pageIsOpen()) {
        const hash = active === sectionDefinitions[0].id ? routeHash : `${routeHash}/${active}`;
        if (window.location.hash !== hash) {
            window.history.replaceState(routeState(true), '', routeURL(hash));
        }
    }
    if (snapshot) render();
}

function sectionFromHash() {
    const match = /^#system-info\/([a-z]+)$/.exec(window.location.hash);
    return match && sectionIDs.has(match[1]) ? match[1] : sectionDefinitions[0].id;
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
        button.append(icon(section.icon, 'system-info-tab-icon'),
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
    // An explicit confirmation outranks the polling status, otherwise the next
    // automatic refresh wipes the feedback before it can be read.
    if (confirmation) return;
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

async function copyText(text) {
    try {
        if (navigator.clipboard?.writeText) {
            await navigator.clipboard.writeText(text);
            return true;
        }
    } catch (_error) { /* fall through to the legacy path */ }
    try {
        const scratch = node('textarea', 'sr-only');
        scratch.value = text;
        document.body.append(scratch);
        scratch.select();
        const ok = document.execCommand('copy');
        scratch.remove();
        return ok;
    } catch (_error) {
        return false;
    }
}

function summaryText(data) {
    const lines = [`# ${data.system?.hostname || label('unnamed_system')}`];
    const identity = [data.system?.manufacturer, data.system?.family, data.system?.product].filter(present).join(' · ');
    if (identity) lines.push(identity);
    for (const [name, value] of [
        [label('os'), data.system?.os], [label('kernel'), data.system?.kernel],
        [label('architecture'), data.system?.architecture], [label('uptime'), data.live?.sys?.uptime_human],
    ]) if (present(value)) lines.push(`- ${name}: ${value}`);
    if (data.cpu?.model_name) {
        lines.push('', `## ${label('processor')}`, `- ${data.cpu.model_name}`,
            `- ${number(data.cpu.cores, '', 0)} ${label('cores')} · ${number(data.cpu.logical_cpus, '', 0)} ${label('logical_cpus')}`);
    }
    if (data.filesystems?.length) {
        lines.push('', `## ${label('mounted_storage')}`);
        for (const fs of [...data.filesystems].sort((a, b) => compareMounts(a.mount || '', b.mount || ''))) {
            const usage = fs.usage ? ` — ${bytes(fs.usage.used)} ${label('used').toLowerCase()} / ${bytes(fs.usage.total)} (${percent(fs.usage.used_pct)})` : '';
            lines.push(`- \`${fs.mount}\` (${fs.type || '?'}, ${fs.device || '?'})${usage}`);
        }
    }
    const drives = (data.disks || []).filter(disk => disk.class === 'disk');
    if (drives.length) {
        lines.push('', `## ${label('physical_drives')}`);
        for (const disk of drives) {
            lines.push(`- ${disk.details?.model || disk.name} (\`/dev/${disk.name}\`, ${bytes(disk.size_bytes)}, ${rawLabel(`medium_${disk.medium}`, disk.class || '')})`);
        }
    }
    if (data.network?.length) {
        lines.push('', `## ${label('network')}`);
        for (const iface of data.network) {
            lines.push(`- ${iface.name} (${interfaceKindLabel(iface)})${iface.addresses?.length ? `: ${iface.addresses.join(', ')}` : ''}`);
        }
    }
    if (data.sensors?.length) {
        lines.push('', `## ${label('sensors')}`);
        for (const sensor of data.sensors) {
            lines.push(`- ${deviceLabel(sensor.device)} · ${sensor.name}: ${number(sensor.value, ` ${sensor.unit}`)}`);
        }
    }
    return lines.join('\n');
}

function reportCopy(ok) {
    clearTimeout(confirmation);
    setStatus(label(ok ? 'copied' : 'copy_failed'), ok ? 'live' : 'error');
    confirmation = setTimeout(() => {
        confirmation = null;
        snapshotStatus();
    }, 2500);
}

function stop() {
    clearTimeout(timer);
    timer = null;
    clearTimeout(confirmation);
    confirmation = null;
    request?.abort();
    request = null;
    if (el('system-info-refresh')) el('system-info-refresh').disabled = false;
}

function hideSystemInfo({ clear = true, restoreScroll = true } = {}) {
    const wasOpen = pageIsOpen();
    const restoreFocus = wasOpen && el('system-info-page')?.contains(document.activeElement);
    stop();
    el('system-info-page')?.classList.add('hidden');
    el('system-info-page')?.setAttribute('aria-hidden', 'true');
    el('dashboard-home')?.classList.remove('hidden');
    el('dashboard-home')?.removeAttribute('aria-hidden');
    el('dashboard')?.classList.remove('system-info-active');
    el('btn-info')?.classList.remove('system-info-current');
    el('btn-info')?.removeAttribute('aria-current');
    el('btn-info')?.setAttribute('aria-expanded', 'false');
    el('dashboard-home')?.removeAttribute('inert');
    if (clear) {
        snapshot = null;
        renderedSection = null;
        structure = '';
        expandedSections.clear();
        storageQuery = '';
        deviceQuery = '';
        el('system-info-content')?.replaceChildren();
        setStatus('');
    }
    if (restoreFocus && enabled) el('btn-info')?.focus({ preventScroll: true });
    if (wasOpen && restoreScroll) {
        requestAnimationFrame(() => {
            window.scrollTo({ top: dashboardScroll, behavior: 'instant' });
            window.dispatchEvent(new Event('resize'));
        });
    }
}

export function closeSystemInfo({ useHistory = true } = {}) {
    const markedRoute = window.location.hash.startsWith(routeHash) &&
        window.history.state?.kulaPage === 'system-info';
    hideSystemInfo();
    if (!window.location.hash.startsWith(routeHash)) return;
    if (useHistory && markedRoute) window.history.back();
    else window.history.replaceState(routeState(false), '', routeURL());
}

export function showSystemInfo({ pushRoute = true, focusTitle = true } = {}) {
    if (!enabled) return;
    const firstOpen = !pageIsOpen();
    if (firstOpen) dashboardScroll = window.scrollY;
    el('dashboard-home')?.classList.add('hidden');
    el('dashboard-home')?.setAttribute('aria-hidden', 'true');
    el('dashboard-home')?.setAttribute('inert', '');
    el('system-info-page')?.classList.remove('hidden');
    el('system-info-page')?.removeAttribute('aria-hidden');
    el('dashboard')?.classList.add('system-info-active');
    el('btn-info')?.classList.add('system-info-current');
    el('btn-info')?.setAttribute('aria-current', 'page');
    el('btn-info')?.setAttribute('aria-expanded', 'true');
    active = sectionFromHash();
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
        const next = structureKey(data);
        const rebuild = next !== structure || renderedSection !== active;
        snapshot = data;
        structure = next;
        snapshotStatus();
        if (rebuild) render();
        else patchValues(snapshot);
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
            if (pageIsOpen() && !document.hidden) timer = setTimeout(refresh, pollInterval);
        }
    }
}

export function initSystemInfo() {
    tabs();
    el('btn-info').addEventListener('click', () => pageIsOpen() ? closeSystemInfo() : showSystemInfo());
    el('system-info-back').addEventListener('click', () => closeSystemInfo());
    el('system-info-refresh').addEventListener('click', refresh);
    el('system-info-copy')?.addEventListener('click', async () => {
        if (!snapshot) return;
        reportCopy(await copyText(summaryText(snapshot)));
    });
    // Paths and identity values are pasted into tickets and chat; make them
    // copyable without turning every one of them into a tab stop.
    el('system-info-content').addEventListener('click', async event => {
        const target = event.target.closest('[data-copy]');
        if (!target || !el('system-info-content').contains(target)) return;
        reportCopy(await copyText(target.dataset.copy));
    });
    window.addEventListener('popstate', () => {
        if (window.location.hash.startsWith(routeHash) && enabled) {
            const section = sectionFromHash();
            showSystemInfo({ pushRoute: false, focusTitle: false });
            if (section !== active) activateSection(section, { updateRoute: false });
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
        else if (window.location.hash.startsWith(routeHash) && !pageIsOpen()) {
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
