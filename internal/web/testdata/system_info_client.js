// Runs inside the real dashboard fixture after the historical chart regressions.
export async function testSystemInfo() {
    const check = (condition, message) => { if (!condition) throw new Error(message); };
    const waitFor = async predicate => {
        for (let i = 0; i < 100; i++) { if (predicate()) return; await new Promise(resolve => setTimeout(resolve, 10)); }
        throw new Error('System info request timed out');
    };
    const { state } = await import('./js/app/state.js');
    const { closeSystemInfo } = await import('./js/app/system-info.js');
    const buffer = state.dataBuffer, range = state.customFrom;
    const fetchBefore = window.fetch, timeoutBefore = window.setTimeout, clearBefore = window.clearTimeout;
    const scheduled = new Map();
    let timerID = 1000000, requests = 0, fail = false, pending = false;
    const fixture = {
        ts: new Date().toISOString(), metrics_ts: new Date().toISOString(),
        system: { hostname: 'current-host', os: 'Debian GNU/Linux 13', kernel: '6.12.0-amd64', architecture: 'amd64', manufacturer: 'Example Systems', product: 'Rack Server R2' },
        board: { manufacturer: 'Example Systems', model: '<img src=x onerror=window.inventoryXSS=true>', version: 'Rev 2.1', serial: 'BOARD-001' },
        bios: { vendor: 'Example Firmware', version: '2.4.0', date: '08/12/2026' },
        cpu: { details: { model_name: 'Example 16-Core Processor', vendor: 'GenuineIntel', online: '0-31', features: 'sse, avx, avx2, aes', numa_nodes: '0-1' }, logical_cpus: 32, cores: 16, sockets: 1,
            caches: [{ level: '3', type: 'Unified', size: '32M', shared_cpus: '0-31' }], frequency: [], vulnerabilities: { spectre_v2: 'Mitigation: Enhanced IBRS' } },
        memory: { MemTotal: '67108864 kB', MemAvailable: '45088768 kB', HugePages_Total: '0' },
        dimms: [{ slot: 'DIMM_A1', manufacturer: 'Example Memory', size_mib: '32768', type: 'DDR5', configured_speed_mts: '5600' }],
        disks: [{ name: 'nvme0n1', size_bytes: 2000000000000, details: { model: 'Example NVMe 2TB', serial: 'NVME-001', type: 'SSD / flash', logical_sector_bytes: '512' },
            slaves: [], mounts: ['/'], read_bps: 20971520, write_bps: 5242880, reads_ps: 240, writes_ps: 80, busy_pct: 18, in_flight: 2 }],
        filesystems: [{ device: '/dev/nvme0n1p2', mount: '/', type: 'ext4', options: 'rw,relatime', usage: { total: 2000000000000, used: 800000000000, available: 1100000000000, used_pct: 40 } }],
        network: [{ name: 'eth0', details: { state: 'up', mac: '02:00:00:00:00:01', mtu: '1500', duplex: 'full', driver: 'igc' }, addresses: ['192.0.2.10/24', '2001:db8::10/64'], speed_mbps: 1000,
            rx_mbps: 125, tx_mbps: 42, rx_pct: 12.5, tx_pct: 4.2, rx_bytes: 9876543210, tx_bytes: 1234567890, rx_errors: 0, tx_errors: 0, rx_dropped: 0, tx_dropped: 0 },
            { name: 'lo', details: { state: 'unknown', mtu: '65536' }, addresses: ['127.0.0.1/8'], rx_mbps: 0, tx_mbps: 0 }],
        pci: [{ address: '0000:01:00.0', vendor_id: '0x8086', device_id: '0x1234', driver: 'igc', class: '0x020000' }],
        usb: [{ address: '1-1', product: 'USB Keyboard', vendor_id: '046d' }],
        sensors: [{ device: 'coretemp', name: 'Package', value: 48, unit: '°C' }, { device: 'board', name: 'CPU fan', value: 1240, unit: 'RPM' }],
        power: [{ name: 'UPS', manufacturer: 'Example Power', status: 'Full', capacity_percent: '100' }],
        live: { cpu: { total: { usage: 37, user: 25, system: 12, iowait: 0, steal: 0 }, temp: 48 }, mem: { total: 68719476736, used: 22548578304, available: 46170898432, used_pct: 32.8 },
            swap: { total: 8589934592, used: 0, free: 8589934592, used_pct: 0 },
            sys: { uptime_human: '12d 4h 18m', clock_synced: true, clock_source: 'tsc', user_count: 2 },
            proc: { total: 241, threads: 1280, running: 2, blocked: 0, zombie: 0 }, lavg: { load1: 1.2, load5: 0.9, load15: 0.8 }, gpu: [] },
    };
    window.setTimeout = (callback, ms, ...args) => {
        if (ms === 5000) { const id = ++timerID; scheduled.set(id, callback); return id; }
        return timeoutBefore(callback, ms, ...args);
    };
    window.clearTimeout = id => { if (!scheduled.delete(id)) clearBefore(id); };
    window.fetch = (url, options) => {
        if (!String(url).includes('/api/system-info')) return fetchBefore(url, options);
        check(String(url) === '/fixture/api/system-info', 'System info ignored base path or included history parameters');
        check(options.cache === 'no-store', 'System info requests permit stale browser caching');
        requests++;
        if (pending) return new Promise((_resolve, reject) => options.signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError'))));
        return Promise.resolve(new Response(JSON.stringify(fixture), { status: fail ? 500 : 200, headers: { 'Content-Type': 'application/json' } }));
    };
    const page = document.getElementById('system-info-page'), home = document.getElementById('dashboard-home');
    const content = document.getElementById('system-info-content'), infoButton = document.getElementById('btn-info');
    const refresh = document.getElementById('system-info-refresh');
    const select = name => document.querySelector(`[data-section="${name}"]`).click();
    try {
        check(requests === 0, 'Inventory was fetched while closed');
        infoButton.click();
        await waitFor(() => content.textContent.includes('current-host'));
        check(!page.classList.contains('hidden') && home.classList.contains('hidden'), 'System info did not replace the dashboard page');
        check(location.hash === '#system-info' && infoButton.getAttribute('aria-current') === 'page', 'System info page is not reflected in navigation state');
        check(!document.getElementById('system-info-dialog'), 'Obsolete system info dialog is still present');
        check(!page.querySelector('.system-info-hero'), 'Obsolete system info hero is still present');
        const toolbar = page.querySelector('.system-info-toolbar'), tabs = document.getElementById('system-info-tabs');
        check(Boolean(toolbar && (toolbar.compareDocumentPosition(tabs) & Node.DOCUMENT_POSITION_FOLLOWING)),
            'Update status and refresh action are not above the section menu');
        check(content.textContent.includes('<img src=x') && !content.querySelector('img') && !window.inventoryXSS, 'Hardware strings can inject HTML');
        check(content.querySelector('[role="progressbar"][aria-label="CPU usage"]')?.getAttribute('aria-valuenow') === '37', 'System page used historical CPU usage');
        check(content.querySelectorAll('.system-info-summary-card').length === 4, 'At-a-glance summary is incomplete');
        const cardIcons = [...content.querySelectorAll('.system-info-summary-icon, .system-info-card-icon')];
        check(cardIcons.length > 4 && cardIcons.every(item => item.querySelector('svg') && !item.textContent.trim()),
            'System cards still use text badges instead of graphical icons');
        const usageMeter = content.querySelector('.system-info-meter');
        check(parseFloat(getComputedStyle(usageMeter).maxWidth) <= 520,
            'Usage meters do not have a readable maximum width');
        const scrollBeforeSectionChange = window.scrollY;
        select('cpu');
        await new Promise(resolve => requestAnimationFrame(resolve));
        check(window.scrollY === scrollBeforeSectionChange, 'Selecting a section automatically scrolled the page');
        for (const section of ['cpu', 'memory', 'storage', 'network', 'devices', 'sensors']) {
            select(section);
            check(content.textContent.trim().length > 10, `Empty system info section: ${section}`);
            check(!/undefined|NaN|\[object Object\]/.test(content.textContent), `Invalid display in ${section}`);
            check(content.scrollWidth <= content.clientWidth + 1, `Horizontal overflow in ${section}`);
        }
        const temperatureGauge = content.querySelector('.system-info-thermometer[role="meter"]');
        check(Boolean(temperatureGauge?.getAttribute('aria-valuenow') === '48' &&
            temperatureGauge.querySelector('.system-info-thermometer-fill')),
            'Temperature sensors do not have a graphical reading');
        select('network');
        check(content.querySelectorAll('[role="progressbar"]').length === 2, 'Unknown link speed produced fake utilization bars');
        const beforeRefresh = requests;
        fixture.network[0].rx_mbps = 250;
        const [id, tick] = scheduled.entries().next().value;
        scheduled.delete(id); tick();
        await waitFor(() => requests > beforeRefresh && !refresh.disabled);
        check(content.textContent.includes('250 Mb/s'), 'Live polling did not update interface utilization');
        check(document.querySelector('[data-section="network"]').getAttribute('aria-selected') === 'true', 'Refresh lost selected section');
        document.querySelector('[data-section="network"]').dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }));
        check(document.querySelector('[data-section="devices"]').getAttribute('aria-selected') === 'true', 'Arrow keys do not move between system info tabs');
        select('network');
        fail = true; refresh.click();
        await waitFor(() => !refresh.disabled);
        check(content.textContent.includes('250 Mb/s') && document.getElementById('system-info-status').textContent.includes('out of date'), 'Fetch failure silently presents stale information as live');
        fail = false;
        pending = true; refresh.click();
        closeSystemInfo({ useHistory: false });
        await new Promise(resolve => setTimeout(resolve, 0));
        check(page.classList.contains('hidden') && !home.classList.contains('hidden') && !scheduled.size && !content.children.length,
            'Leaving the page did not abort requests, clear readings, and restore the dashboard');
        pending = false;
        document.dispatchEvent(new CustomEvent('kula-config-ready', { detail: { show_system_info: false } }));
        infoButton.click();
        check(page.classList.contains('hidden') && infoButton.classList.contains('hidden'), 'Hidden system information can still open');
        check(state.dataBuffer === buffer && state.customFrom === range, 'System inventory modified history state');
        document.dispatchEvent(new CustomEvent('kula-config-ready', { detail: { show_system_info: true } }));
        infoButton.click();
        await waitFor(() => content.children.length);
        page.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
        check(!page.classList.contains('hidden'), 'A page still behaves like a dismissible modal');
        document.getElementById('system-info-back').click();
        await waitFor(() => page.classList.contains('hidden') && location.hash !== '#system-info');
        check(!content.children.length && !infoButton.hasAttribute('aria-current'),
            'Back navigation did not leave the System Info page cleanly');
    } finally {
        closeSystemInfo({ useHistory: false });
        window.fetch = fetchBefore; window.setTimeout = timeoutBefore; window.clearTimeout = clearBefore;
        document.dispatchEvent(new CustomEvent('kula-config-ready', { detail: { show_system_info: true } }));
    }
    // Optional visual QA uses the same deterministic hardware fixture.
    window.openSystemInfoQA = async () => {
        window.fetch = (url, options) => String(url).includes('/api/system-info')
            ? Promise.resolve(new Response(JSON.stringify(fixture))) : fetchBefore(url, options);
        infoButton.click();
        await waitFor(() => content.children.length);
        select('overview');
    };
}
