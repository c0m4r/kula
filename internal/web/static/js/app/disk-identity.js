// Keep legacy name-based history separate from identified physical disks.
export function diskKey(disk) {
    return disk ? disk.id || `kernel:${disk.name}` : null;
}

export function diskMember(disks, key) {
    return key && Array.isArray(disks) ? disks.find(disk => diskKey(disk) === key) : undefined;
}

// This module imports nothing (tests load it on its own), so the dashboard
// hands it a translator; until then, and for a key it lacks, English is used.
const ENGLISH = {
    disk_unstable_name: '{name} (unstable)',
    disk_no_identity: 'No persistent identity; this kernel name may refer to a different drive after reboot.',
};
let translate = null;

export function setDiskIdentityTranslator(fn) {
    translate = fn;
}

function text(key) {
    const translated = translate?.(key);
    return translated && translated !== key ? translated : ENGLISH[key];
}

export function diskLabel(disk) {
    return disk?.id ? disk.name : text('disk_unstable_name').replaceAll('{name}', () => disk?.name || '');
}

export function diskTitle(disk) {
    return disk?.id || text('disk_no_identity');
}

// Encode every code point: replacing punctuation with '_' would collide for
// distinct serials (and stable IDs contain punctuation by design).
export function diskDOMKey(key) {
    return Array.from(key, ch => ch.codePointAt(0).toString(16)).join('-');
}

export function migrateDiskSelection(selected, disks) {
    if (!selected) return disks.length ? diskKey(disks[0]) : '';
    // Only old, bare kernel-name preferences are migrated. An absent stable
    // selection must survive hot unplug or browsing a period before it existed.
    if (selected.includes(':')) return selected;
    const disk = disks.find(disk => disk.name === selected);
    return disk ? diskKey(disk) : `kernel:${selected}`;
}
