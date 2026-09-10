/* Current hardware inventory has its own request lifecycle. Chart history,
   pause state, and WebSocket buffers never supply values to this page. */
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
const routeHash = '#system-info';
let active = 'overview';
let snapshot = null;
let timer = null;
let request = null;
let enabled = true;
let dashboardScroll = 0;
const expandedSections = new Map();
let renderedSection = null;
let storageQuery = '';

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

const label = key => i18n.translations[`si_${normalizedKey(key)}`] ||
    i18n.translations[normalizedKey(key)] || humanize(key);
const number = (value, unit = '', digits = 1) => present(value) && finite(Number(value))
    ? `${Number(value).toLocaleString(i18n.currentLang, { maximumFractionDigits: digits })}${unit}` : '—';
const bytes = value => present(value) && finite(Number(value)) ? formatBytesShort(Number(value)) : '—';
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

function card(title, values, options = {}) {
    const item = node('section', `system-info-card${options.wide ? ' system-info-wide' : ''}`);
    const heading = node('header', 'system-info-card-heading');
    if (options.icon) heading.append(icon(options.icon, 'system-info-card-icon'));
    const text = node('div');
    text.append(node('h4', '', title));
    if (options.subtitle) text.append(node('p', '', options.subtitle));
    heading.append(text);
    if (present(options.state)) {
        heading.append(node('span', `system-info-state ${isOnline(options.state) ? 'is-online' : isOffline(options.state) ? 'is-offline' : ''}`,
            translatedState(options.state)));
    }
    item.append(heading);
    if (values) item.append(details(values));
    return item;
}

function expandable(key, title, child) {
    const wrap = node('details', 'system-info-expandable');
    wrap.dataset.key = key;
    wrap.append(node('summary', '', title), child);
    return wrap;
}

function callout(text) {
    return node('p', 'system-info-note system-info-wide', text);
}

function summaryCard(iconName, title, value, detail) {
    const linked = sectionDefinitions.some(section => section.id === iconName);
    const item = node(linked ? 'button' : 'article', `system-info-summary-card${linked ? ' is-link' : ''}`);
    if (linked) {
        item.type = 'button';
        item.dataset.summarySection = iconName;
        item.addEventListener('click', () => activateSection(iconName, { focus: true }));
    }
    const top = node('div', 'system-info-summary-top');
    top.append(icon(iconName, 'system-info-summary-icon'), node('span', 'system-info-summary-title', title));
    item.append(top, node('strong', 'system-info-summary-value', value),
        node('span', 'system-info-summary-detail', detail));
    return item;
}

function profile(data) {
    const system = data.system || {};
    const item = node('section', 'system-info-profile system-info-wide');
    const primary = node('div');
    primary.append(node('h4', '', system.hostname || label('unnamed_system')));
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
    for (const [name, value] of [
        [label('kernel'), system.kernel],
        [label('uptime'), data.live?.sys?.uptime_human],
    ]) {
        const fact = node('div', 'system-info-profile-fact');
        fact.append(node('span', '', name), node('strong', '', value || '—'));
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
        summaryCard('cpu', label('processor'), cpu.model_name || label('unavailable'),
            [
                coreCount ? `${number(coreCount, '', 0)} ${label('cores')}` : '',
                cpu.logical_cpus ? `${number(cpu.logical_cpus, '', 0)} ${label('logical_cpus')}` : '',
            ].filter(Boolean).join(' · ')),
        summaryCard('memory', i18n.t('ram'), bytes(live?.mem?.total),
            ''),
        summaryCard('storage', label('main_storage'), bytes(filesystem?.usage?.total),
            filesystem?.mount || label('storage_usage_unavailable')),
        summaryCard('network', label('network'), iface?.name || label('unavailable'),
            iface ? [
                finite(iface.speed_mbps) ? number(iface.speed_mbps, ' Mb/s', 0) : '',
                iface.addresses?.[0],
            ].filter(Boolean).join(' · ') || label('link_details_unavailable') : label('no_interfaces')),
    );
    grid.append(summaries);
}

// A mount path is one value, even when it contains spaces or commas.
function mountPaths(mounts, key) {
    const paths = [...new Set(mounts.filter(present))].sort(compareMounts);
    if (!paths.length) return node('span', '', '—');
    const wrap = node('div', 'system-info-mount-paths');
    const list = values => {
        const items = node('ul', 'system-info-path-list');
        for (const path of values) {
            const item = node('li');
            item.append(node('code', 'system-info-path', path));
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
        head.append(node('code', 'system-info-path', filesystem.mount || label('unknown_mount')));
        const meta = node('div', 'system-info-filesystem-meta');
        if (present(filesystem.type)) meta.append(node('span', 'system-info-fs-type', filesystem.type));
        if (present(filesystem.device)) meta.append(node('code', 'system-info-path', filesystem.device));
        identity.append(head, meta);
        item.append(identity);
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
        const matches = sorted.filter(fs => !query || [fs.mount, fs.device, fs.type, fs.options]
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
    };
    input.addEventListener('input', () => {
        storageQuery = input.value;
        update();
    });
    update();
    item.append(toolbar, results);
    return item;
}

function driveCard(disk) {
    const info = disk.details || {};
    const title = info.model || info.volume_name || disk.name;
    const item = card(title, {
        capacity: disk.size_bytes === undefined ? undefined : bytes(disk.size_bytes),
        drive_type: info.type,
    }, { icon: 'storage', subtitle: `/dev/${disk.name}` });
    if (disk.mounts?.length) {
        const mounts = node('div', 'system-info-drive-mounts');
        mounts.append(node('span', 'system-info-mount-label', label('mounted_at')),
            mountPaths(disk.mounts, `drive-mounts-${disk.name}`));
        item.append(mounts);
    }
    return item;
}

function storage(data, grid) {
    if (data.filesystems?.length) grid.append(mountedStorage(data.filesystems));

    const all = data.disks || [];
    const primary = all.filter(disk => disk.details?.type !== 'partition' && !disk.name.startsWith('loop'));
    const prominent = primary.length ? primary : all.slice(0, 1);
    for (const disk of prominent) grid.append(driveCard(disk));
    if (!all.length) grid.append(callout(label('no_storage_devices')));

}

function interfaceCard(iface) {
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
    return item;
}

function network(data, grid) {
    const all = data.network || [];
    for (const iface of all) grid.append(interfaceCard(iface));
    if (!all.length) grid.append(callout(label('no_interfaces')));

}

function inventoryList() {
    return node('ul', 'system-info-inventory-list system-info-wide');
}

function inventoryItem(iconName, title, subtitle, values, state) {
    const item = node('li', 'system-info-inventory-item');
    item.append(icon(iconName, 'system-info-card-icon'));
    const body = node('div', 'system-info-inventory-body');
    const heading = node('div', 'system-info-inventory-heading');
    const titleWrap = node('div');
    titleWrap.append(node('h4', '', title));
    if (subtitle) titleWrap.append(node('p', '', subtitle));
    heading.append(titleWrap);
    if (present(state)) {
        heading.append(node('span', `system-info-state ${isOnline(state) ? 'is-online' : isOffline(state) ? 'is-offline' : ''}`,
            translatedState(state)));
    }
    body.append(heading);
    if (values && Object.values(values).some(present)) body.append(details(values));
    item.append(body);
    return item;
}

function devices(data, grid) {
    const list = inventoryList();
    for (const [index, gpu] of (data.live?.gpu || []).entries()) {
        list.append(inventoryItem('gpu', gpu.name || `${label('graphics_device')} ${index + 1}`,
            label('graphics_device'), { driver: gpu.driver }));
    }
    for (const [index, usb] of (data.usb || []).entries()) {
        const primary = ['manufacturer', 'speed_mbps', 'max_power'];
        list.append(inventoryItem('usb', usb.product || `${label('usb_device')} ${index + 1}`,
            usb.address ? `${label('port')} ${usb.address}` : label('usb_device'), select(usb, primary)));
    }
    for (const [index, pci] of (data.pci || []).entries()) {
        list.append(inventoryItem('pci', pci.address || `${label('internal_pci_devices')} ${index + 1}`,
            label('internal_pci_devices'), { driver: pci.driver }));
    }
    if (list.children.length) grid.append(list);
    else grid.append(callout(label('no_connected_devices')));
}

function sensors(data, grid) {
    const list = inventoryList();
    for (const sensor of data.sensors || []) {
        list.append(inventoryItem('sensor', sensor.name, humanize(sensor.device), {
            current_reading: number(sensor.value, ` ${sensor.unit}`),
        }));
    }
    for (const [index, power] of (data.power || []).entries()) {
        const primary = ['status', 'capacity_percent'];
        list.append(inventoryItem('power', power.name || `${label('power_supply')} ${index + 1}`,
            label('power_supply'), select(power, primary), power.online));
    }
    if (list.children.length) grid.append(list);
    else grid.append(callout(label('no_sensors_or_power')));
}

const renderers = { overview, storage, network, devices, sensors };

function render() {
    if (!snapshot) return;
    const content = el('system-info-content');
    if (renderedSection) {
        expandedSections.set(renderedSection,
            [...content.querySelectorAll('details[open]')].map(item => item.dataset.key));
    }
    const opened = expandedSections.get(active) || [];
    const focused = renderedSection === active && content.contains(document.activeElement)
        ? document.activeElement?.closest('details')?.dataset.key : null;
    const focusedSearch = renderedSection === active && document.activeElement?.id === 'system-info-mount-search';
    const selection = focusedSearch ? [document.activeElement.selectionStart, document.activeElement.selectionEnd] : null;
    const focusedSummary = renderedSection === active ? document.activeElement?.dataset.summarySection : null;
    renderedSection = active;
    const section = node('div', 'system-info-section');
    const grid = node('div', 'system-info-grid');
    renderers[active](snapshot, grid);
    if (!grid.children.length) {
        grid.append(node('p', 'system-info-empty system-info-wide', label('unavailable')));
    }
    section.append(grid);
    content.replaceChildren(section);
    content.setAttribute('aria-labelledby', `system-info-tab-${active}`);
    content.setAttribute('aria-busy', 'false');
    for (const item of content.querySelectorAll('details')) {
        item.open = opened.includes(item.dataset.key) || Boolean(storageQuery.trim() && item.dataset.key === 'runtime-mounts');
        if (item.dataset.key === focused && !focusedSearch) item.querySelector('summary').focus({ preventScroll: true });
    }
    if (focusedSearch) {
        const input = el('system-info-mount-search');
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

function activateSection(section, { focus = false } = {}) {
    if (!renderers[section]) return;
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
    const restoreFocus = wasOpen && el('system-info-page')?.contains(document.activeElement);
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
        renderedSection = null;
        expandedSections.clear();
        storageQuery = '';
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
    el('btn-info').addEventListener('click', () => pageIsOpen() ? closeSystemInfo() : showSystemInfo());
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
