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
        cpu: { model_name: 'Example 16-Core Processor', logical_cpus: 32, cores: 16 },
        disks: [{ name: 'nvme0n1', size_bytes: 2000000000000, details: { model: 'Example NVMe 2TB', type: 'Non-rotating' },
            slaves: [], mounts: ['/'] }],
        filesystems: [{ device: '/dev/nvme0n1p2', mount: '/', type: 'ext4', options: 'rw,relatime', usage: { total: 2000000000000, used: 800000000000, available: 1100000000000, used_pct: 40 } }],
        network: [{ name: 'eth0', details: { state: 'unknown', driver: 'igc' }, addresses: ['192.0.2.10/24', '2001:db8::10/64'], speed_mbps: 1000 },
            { name: 'lo', details: { state: 'unknown' }, addresses: ['127.0.0.1/8'] }],
        pci: [{ address: '0000:01:00.0', driver: 'igc' }],
        usb: [{ address: '1-1', product: 'USB Keyboard' }],
        sensors: [{ device: 'coretemp', name: 'Package', value: 48, unit: '°C' }, { device: 'board', name: 'CPU fan', value: 1240, unit: 'RPM' }],
        power: [{ name: 'UPS', manufacturer: 'Example Power', status: 'Full', capacity_percent: '100' }],
        live: { mem: { total: 68719476736 }, sys: { uptime_human: '12d 4h 18m' }, gpu: [] },
    };
    const longMount = '/srv/projects/a-very-long-directory-name-with-no-breaks-' + 'archive'.repeat(18) + '/data, current';
    fixture.filesystems.push(
        { device: '/dev/nvme0n1p1', mount: '/boot/efi', type: 'vfat', options: 'rw,relatime',
            usage: { total: 1073741824, used: 104857600, available: 968884224, used_pct: 9.8 } },
        { device: 'nas.example:/archive', mount: longMount, type: 'nfs4', options: 'rw,relatime,vers=4.2' },
        ...Array.from({ length: 18 }, (_, index) => ({ device: 'tmpfs', mount: `/run/services/service-${index}`, type: 'tmpfs', options: 'rw,nosuid,nodev',
            usage: { total: 1073741824, used: 1048576, available: 1072693248, used_pct: 0.1 } })),
    );
    fixture.disks[0].mounts = ['/var/lib/data', '/', '/boot/efi', longMount, '/', '/srv/backups'];
    fixture.disks.push({ name: 'nvme0n1p2', size_bytes: 2000000000000, parent: 'nvme0n1', details: { type: 'partition' },
        mounts: [...fixture.disks[0].mounts] });
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
        check(!page.querySelector('.system-info-availability'), 'Obsolete availability message is still present');
        const toolbar = page.querySelector('.system-info-toolbar'), tabs = document.getElementById('system-info-tabs');
        check(Boolean(toolbar && (toolbar.compareDocumentPosition(tabs) & Node.DOCUMENT_POSITION_FOLLOWING)),
            'Update status and refresh action are not above the section menu');
        check(!content.querySelector('img') && !window.inventoryXSS, 'Hardware strings can inject HTML');
        check(content.querySelectorAll('.system-info-summary-card').length === 4, 'At-a-glance summary is incomplete');
        check(!content.querySelector('.system-info-section-header'), 'System info section header is still present');
        check(!content.querySelector('.system-info-meter, [role="progressbar"]'), 'Overview still displays live usage');
        check(!content.textContent.includes('This server') && !content.textContent.includes('Fast working memory used by running software'),
            'Removed overview labels are still displayed');
        check(content.querySelector('.system-info-profile-side')?.textContent.includes('12d 4h 18m') &&
            content.querySelector('.system-info-profile-side')?.textContent.includes('6.12.0-amd64'),
            'Kernel or uptime is missing from the hostname profile');
        check(!content.querySelector('.system-info-card'), 'Redundant System Info card is still displayed');
        check(!document.querySelector('[data-section="cpu"]') && !document.querySelector('[data-section="memory"]'),
            'CPU or memory tab is still present');
        content.querySelectorAll('.system-info-summary-card')[2].click();
        check(document.querySelector('[data-section="storage"]').getAttribute('aria-selected') === 'true',
            'Storage summary does not open storage details');
        select('overview');
        const cardIcons = [...content.querySelectorAll('.system-info-summary-icon, .system-info-card-icon')];
        check(cardIcons.length === 4 && cardIcons.every(item => item.querySelector('svg') && !item.textContent.trim()),
            'System cards still use text badges instead of graphical icons');
        const scrollBeforeSectionChange = window.scrollY;
        select('network');
        await new Promise(resolve => requestAnimationFrame(resolve));
        check(window.scrollY === scrollBeforeSectionChange, 'Selecting a section automatically scrolled the page');
        for (const section of ['storage', 'network', 'devices', 'sensors']) {
            select(section);
            check(content.textContent.trim().length > 10, `Empty system info section: ${section}`);
            check(!/undefined|NaN|\[object Object\]/.test(content.textContent), `Invalid display in ${section}`);
            check(content.scrollWidth <= content.clientWidth + 1, `Horizontal overflow in ${section}`);
        }
        check(content.querySelector('.system-info-inventory-list')?.tagName === 'UL' &&
            content.querySelectorAll('.system-info-inventory-item').length === 3 &&
            !content.querySelector('.system-info-card'), 'Sensors and power supplies are not rendered as a list');
        select('devices');
        check(content.querySelector('.system-info-inventory-list')?.tagName === 'UL' &&
            content.querySelectorAll('.system-info-inventory-item').length === 2 &&
            !content.querySelector('.system-info-card'), 'Connected devices are not rendered as a list');
        select('storage');
        const mountCard = content.querySelector('.system-info-mounted-storage');
        check(mountCard.querySelector('.system-info-path').textContent === '/', 'Root mount is not first');
        check(!mountCard.querySelector('[data-key="runtime-mounts"]').open, 'Runtime mounts clutter the main storage list');
        check([...mountCard.querySelectorAll('.system-info-filesystem')].filter(row => row.checkVisibility()).length === 3,
            'Primary mounts are missing or runtime mounts are not collapsed');
        check(mountCard.textContent.includes(longMount), 'Long mount paths lost information');
        const drivePaths = content.querySelector('.system-info-drive-mounts');
        check(drivePaths.querySelectorAll('li').length === 5, 'Drive mounts were not deduplicated');
        check([...drivePaths.querySelectorAll('li')].filter(row => row.checkVisibility()).length === 3,
            'Long drive mount lists are not compact');
        drivePaths.querySelector('details').open = true;
        check([...drivePaths.querySelectorAll('code')].some(path => path.textContent === longMount),
            'Mount paths containing commas are split or truncated');
        const searchMounts = query => {
            const input = document.getElementById('system-info-mount-search');
            input.value = query;
            input.dispatchEvent(new Event('input', { bubbles: true }));
        };
        searchMounts('service-17');
        check(mountCard.querySelectorAll('.system-info-filesystem').length === 1 &&
            mountCard.querySelector('[data-key="runtime-mounts"]').open, 'Search does not reveal matching runtime mounts');
        searchMounts('NFS4');
        const searchInput = document.getElementById('system-info-mount-search');
        searchInput.focus();
        searchInput.setSelectionRange(1, 3);
        const beforeMountRefresh = requests;
        const [mountTimerID, mountTick] = scheduled.entries().next().value;
        scheduled.delete(mountTimerID); mountTick();
        await waitFor(() => requests > beforeMountRefresh && !refresh.disabled);
        const refreshedSearch = document.getElementById('system-info-mount-search');
        check(refreshedSearch.value === 'NFS4' && document.activeElement === refreshedSearch && refreshedSearch.selectionStart === 1 && refreshedSearch.selectionEnd === 3,
            'Live refresh interrupts mount search');
        check(content.querySelector('.system-info-mounted-storage').querySelectorAll('.system-info-filesystem').length === 1,
            'Refresh loses the mount filter');
        searchMounts('mount-that-does-not-exist');
        check(content.textContent.includes('No mountpoints match your search.'), 'Empty mount search has no feedback');
        searchMounts('');
        select('storage');
        check(content.querySelector('.system-info-drive-mounts details').open, 'Mount expansion was lost when changing sections');
        select('network');
        const unknownState = [...content.querySelectorAll('.system-info-state')].find(item => item.textContent === 'unknown');
        check(unknownState && !unknownState.classList.contains('is-offline'), 'Unknown interface state is styled as offline');
        check(!content.querySelector('[role="progressbar"]') && !content.textContent.includes('Receiving now'),
            'Network live activity is still displayed');
        const beforeRefresh = requests;
        fixture.network[0].rx_mbps = 250;
        const [id, tick] = scheduled.entries().next().value;
        scheduled.delete(id); tick();
        await waitFor(() => requests > beforeRefresh && !refresh.disabled);
        check(!content.textContent.includes('250 Mb/s'), 'Live interface utilization is still displayed');
        check(document.querySelector('[data-section="network"]').getAttribute('aria-selected') === 'true', 'Refresh lost selected section');
        document.querySelector('[data-section="network"]').dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }));
        check(document.querySelector('[data-section="devices"]').getAttribute('aria-selected') === 'true', 'Arrow keys do not move between system info tabs');
        select('network');
        fail = true; refresh.click();
        await waitFor(() => !refresh.disabled);
        check(document.getElementById('system-info-status').textContent.includes('out of date'), 'Fetch failure silently presents stale information as live');
        fail = false;
        fixture.live.mem.total = null;
        refresh.click();
        await waitFor(() => !refresh.disabled);
        select('overview');
        check(content.querySelectorAll('.system-info-summary-value')[1].textContent === '—', 'Missing memory is displayed as zero');
        fixture.live.mem.total = 68719476736;
        content.querySelector('[data-summary-section="storage"]').focus();
        const [summaryTimerID, summaryTick] = scheduled.entries().next().value;
        scheduled.delete(summaryTimerID); summaryTick();
        await waitFor(() => !refresh.disabled);
        check(document.activeElement?.dataset.summarySection === 'storage', 'Live refresh loses overview keyboard focus');
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
        infoButton.click();
        await waitFor(() => page.classList.contains('hidden') && location.hash !== '#system-info');
        check(!infoButton.hasAttribute('aria-current'), 'System info icon did not return to the dashboard');
        infoButton.click();
        await waitFor(() => content.children.length);
        page.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
        check(!page.classList.contains('hidden'), 'A page still behaves like a dismissible modal');
        document.getElementById('system-info-back').focus();
        document.getElementById('system-info-back').click();
        await waitFor(() => page.classList.contains('hidden') && location.hash !== '#system-info');
        check(document.activeElement === infoButton, 'Back navigation left keyboard focus inside the hidden page');
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
