/* ============================================================
   verify-lock.js — supply-chain checks on the bundle's lockfile.
   Usage: node verify-lock.js <package-lock.json> <min-release-age-days>

   npm ci installs locked versions without applying min-release-age,
   so every locked package is checked here before anything installs:
   exact direct pins, tarballs from registry.npmjs.org with sha512
   integrity, and a publish date at least the given days old.
   ============================================================ */
'use strict';
import { readFile } from 'node:fs/promises';

const REGISTRY = 'https://registry.npmjs.org/';
const [lockPath, days] = process.argv.slice(2);
const minDays = Number(days);
if (!lockPath || !(minDays > 0)) {
    console.error('usage: node verify-lock.js <package-lock.json> <min-release-age-days>');
    process.exit(2);
}

const lock = JSON.parse(await readFile(lockPath, 'utf8'));
const root = lock.packages?.[''] ?? {};
const problems = [];

for (const [name, spec] of Object.entries({ ...root.dependencies, ...root.devDependencies })) {
    if (!/^\d+\.\d+\.\d+$/.test(spec)) problems.push(`${name}: "${spec}" is not an exact release version (x.y.z)`);
}

const cutoff = Date.now() - minDays * 864e5;
const locked = Object.entries(lock.packages ?? {})
    .filter(([path]) => path)
    .map(([path, meta]) => ({ ...meta, name: path.slice(path.lastIndexOf('node_modules/') + 13) }));

await Promise.all(locked.map(async ({ name, version, resolved, integrity }) => {
    const id = `${name}@${version}`;
    if (!resolved?.startsWith(`${REGISTRY}${name}/-/`)) problems.push(`${id}: resolved from ${resolved}`);
    if (!integrity?.startsWith('sha512-')) problems.push(`${id}: no sha512 integrity`);
    try {
        const response = await fetch(REGISTRY + encodeURIComponent(name),{ signal: AbortSignal.timeout(60000) });
        if (!response.ok) throw new Error(`registry answered ${response.status}`);
        const published = Date.parse((await response.json()).time?.[version]);
        if (!Number.isFinite(published)) problems.push(`${id}: no publish time in the registry`);
        else if (published > cutoff) {
            problems.push(`${id}: published ${new Date(published).toISOString().slice(0, 10)}, under ${minDays} days ago`);
        }
    } catch (error) {
        problems.push(`${id}: ${error.message}`);
    }
}));

if (problems.length) {
    console.error(`package-lock.json failed supply-chain checks:\n  ${problems.sort().join('\n  ')}`);
    process.exit(1);
}
console.log(`${locked.length} locked packages pinned, from ${REGISTRY}, and at least ${minDays} days old`);
