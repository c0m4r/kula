/* ============================================================
   verify-lock.js — supply-chain checks on the bundle's lockfile.
   Usage: node verify-lock.js <package-lock.json> <min-release-age-days>

   npm ci installs locked versions without applying min-release-age,
   so every locked package is checked here before anything installs:
   exact direct pins; a `resolved` tarball URL and sha512 `integrity`
   that are exactly the ones registry.npmjs.org lists for the locked
   name and version, so npm fetches and accepts that release and no
   other; and a publish date for that release at least the given
   days old.
   ============================================================ */
'use strict';
import { readFile } from 'node:fs/promises';

const REGISTRY = 'https://registry.npmjs.org/';
// A package name: an optional @scope/ and a name, neither of which can be a
// dot segment or hold a slash, a percent sign or anything else a URL decodes.
const NAME = '(?:@[A-Za-z0-9~-][A-Za-z0-9._~-]*/)?[A-Za-z0-9~-][A-Za-z0-9._~-]*';
const PATH = new RegExp(`^node_modules/(?:${NAME}/node_modules/)*(${NAME})$`);
const [lockPath, days] = process.argv.slice(2);
const minDays = Number(days);
if (!lockPath || !(minDays > 0)) {
    console.error('usage: node verify-lock.js <package-lock.json> <min-release-age-days>');
    process.exit(2);
}

const lock = JSON.parse(await readFile(lockPath, 'utf8'));
// Only lockfile v2+ lists every installed package under `packages`; an older
// or truncated lock would leave nothing to check and pass vacuously.
const packages = lock.packages;
if (!(lock.lockfileVersion >= 2) || !packages || typeof packages !== 'object' || !packages['']) {
    console.error(`${lockPath}: lockfileVersion ${lock.lockfileVersion} has no "packages" map with a root entry; ` +
        'regenerate it with npm 7 or later');
    process.exit(1);
}
const root = packages[''];
const problems = [];

// Every direct dependency, whatever its kind, is an exact pin with a locked entry.
for (const kind of ['dependencies', 'devDependencies', 'optionalDependencies', 'peerDependencies']) {
    for (const [name, spec] of Object.entries(root[kind] ?? {})) {
        if (!/^\d+\.\d+\.\d+$/.test(spec)) problems.push(`${name}: "${spec}" is not an exact release version (x.y.z)`);
        if (!packages[`node_modules/${name}`]) problems.push(`${name}: in ${kind} but not locked`);
    }
}

// A property of a registry document, never one inherited from Object.prototype.
const own = (object, key) =>
    object && typeof object === 'object' && typeof key === 'string' && Object.hasOwn(object, key) ? object[key] : undefined;

// The URL npm would actually fetch: it parses `resolved` with new URL(), which
// resolves dot segments, %2e included, and turns backslashes into slashes.
const canonical = (url) => {
    try {
        return new URL(url).href;
    } catch {
        return undefined;
    }
};

const cutoff = Date.now() - minDays * 864e5;
const locked = Object.entries(packages).filter(([path]) => path);

await Promise.all(locked.map(async ([path, meta]) => {
    const name = PATH.exec(path)?.[1];
    if (!name || !meta || typeof meta !== 'object') {
        problems.push(`${path}: not a node_modules path of a registry package`);
        return;
    }
    const { version, resolved, integrity } = meta;
    const id = `${name}@${version}`;
    // An npm alias ("name" in the entry) installs another package under this name.
    if (meta.name !== undefined && meta.name !== name) problems.push(`${id}: installs the package "${meta.name}"`);
    if (meta.link) problems.push(`${id}: a link to ${resolved}, not a registry tarball`);
    if (typeof resolved !== 'string' || !resolved.startsWith(`${REGISTRY}${name}/-/`)) {
        problems.push(`${id}: resolved from ${resolved}`);
    } else if (/[?#]/.test(resolved) || canonical(resolved) !== resolved) {
        problems.push(`${id}: resolved URL ${JSON.stringify(resolved)} is not in canonical form`);
    }
    if (typeof integrity !== 'string' || !integrity.startsWith('sha512-')) problems.push(`${id}: no sha512 integrity`);
    try {
        const response = await fetch(REGISTRY + encodeURIComponent(name), { signal: AbortSignal.timeout(60000) });
        if (!response.ok) throw new Error(`registry answered ${response.status}`);
        const packument = await response.json();
        // npm fetches `resolved` and accepts it when it matches `integrity`: both
        // must be the registry's own for this version, or the lockfile installs
        // a tarball other than the release whose age is checked below.
        const dist = own(own(own(packument, 'versions'), version), 'dist');
        if (!dist) {
            problems.push(`${id}: no such release in the registry`);
        } else {
            const tarball = own(dist, 'tarball');
            if (tarball !== resolved) problems.push(`${id}: resolved ${resolved}, but the registry lists ${tarball}`);
            if (own(dist, 'integrity') !== integrity) problems.push(`${id}: integrity differs from the registry's`);
        }
        const published = Date.parse(own(own(packument, 'time'), version));
        if (!Number.isFinite(published)) problems.push(`${id}: no publish time in the registry`);
        else if (published > cutoff) {
            problems.push(`${id}: published ${new Date(published).toISOString().slice(0, 10)}, under ${minDays} days ago`);
        }
    } catch (error) {
        problems.push(`${id}: ${error.message}`);
    }
}));

if (locked.length === 0) problems.push('no locked packages');
if (problems.length) {
    console.error(`package-lock.json failed supply-chain checks:\n  ${problems.sort().join('\n  ')}`);
    process.exit(1);
}
console.log(`${locked.length} locked packages pinned to the registry's own tarballs and integrity, ` +
    `and at least ${minDays} days old`);
