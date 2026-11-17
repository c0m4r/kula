/* ============================================================
   verify-lock.test.js — offline tests of verify-lock.js.
   Usage: node addons/chartjs/verify-lock.test.js

   verify-lock.js runs as addons/build-chartjs.sh runs it, with fetch
   replaced by a stand-in registry that lists the committed lockfile's
   releases as published long ago, plus a fresh esbuild release and
   unrelated packages for crafted lockfiles to point at. Nothing is
   fetched from the network.
   ============================================================ */
'use strict';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test, { after } from 'node:test';
import { fileURLToPath, pathToFileURL } from 'node:url';

const REGISTRY = 'https://registry.npmjs.org/';
const VERIFY_LOCK = fileURLToPath(new URL('verify-lock.js', import.meta.url));
const COMMITTED = JSON.parse(readFileSync(new URL('package-lock.json', import.meta.url), 'utf8'));

const LONG_AGO = '2020-01-01T00:00:00.000Z';
const YESTERDAY = new Date(Date.now() - 864e5).toISOString();
const fakeIntegrity = (letter) => `sha512-${letter.repeat(86)}==`;

// The stand-in registry: package name -> packument (versions[v].dist, time[v]).
const registry = {};
const publish = (name, version, time, tarball, integrity) => {
    const packument = (registry[name] ??= { name, versions: {}, time: {} });
    packument.versions[version] = { name, version, dist: { tarball, integrity } };
    packument.time[version] = time;
};
const tarballOf = (name, version) => `${REGISTRY}${name}/-/${name.split('/').pop()}-${version}.tgz`;
for (const [path, entry] of Object.entries(COMMITTED.packages)) {
    if (path) publish(path.slice(path.lastIndexOf('node_modules/') + 13), entry.version, LONG_AGO, entry.resolved, entry.integrity);
}
publish('esbuild', '0.29.9', YESTERDAY, tarballOf('esbuild', '0.29.9'), fakeIntegrity('E'));
publish('evil-esbuild-linux-x64', '1.0.0', YESTERDAY, tarballOf('evil-esbuild-linux-x64', '1.0.0'), fakeIntegrity('A'));
// Published long ago, so only the new checks can refuse a lockfile that points at them.
publish('evil-chart', '1.0.0', LONG_AGO, tarballOf('evil-chart', '1.0.0'), fakeIntegrity('C'));
publish('evil', '1.0.0', LONG_AGO, tarballOf('evil', '1.0.0'), fakeIntegrity('V'));

const work = mkdtempSync(join(tmpdir(), 'kula-verify-lock-'));
after(() => rmSync(work, { recursive: true, force: true }));
const registryFile = join(work, 'registry.json');
writeFileSync(registryFile, JSON.stringify(registry));
const stub = join(work, 'registry-stub.mjs');
writeFileSync(stub, `
import { readFileSync } from 'node:fs';
const registry = JSON.parse(readFileSync(${JSON.stringify(registryFile)}, 'utf8'));
globalThis.fetch = async (url) => {
    url = String(url);
    if (!url.startsWith(${JSON.stringify(REGISTRY)})) throw new Error('stub registry: unexpected fetch of ' + url);
    const name = decodeURIComponent(url.slice(${REGISTRY.length}));
    if (!Object.hasOwn(registry, name)) return new Response('{"error":"Not found"}', { status: 404 });
    return new Response(JSON.stringify(registry[name]), { headers: { 'content-type': 'application/json' } });
};
`);

let runs = 0;
const verify = (lock) => {
    const file = join(work, `lock-${++runs}.json`);
    writeFileSync(file, JSON.stringify(lock, null, 2));
    return spawnSync(process.execPath, ['--import', pathToFileURL(stub).href, VERIFY_LOCK, file, '14'], {
        encoding: 'utf8',
        timeout: 60000,
    });
};

// A copy of the committed lockfile with `change` applied to its packages map.
const craft = (change) => {
    const lock = structuredClone(COMMITTED);
    change(lock.packages, lock);
    return lock;
};
const entry = (packages, name) => packages[`node_modules/${name}`];
const chart = (packages) => entry(packages, 'chart.js');

// The releases the committed lockfile pins, as the refusals below name them,
// so that bumping one (addons/build-chartjs.sh --update) leaves them valid.
const lockedChart = chart(COMMITTED.packages);
const lockedEsbuild = entry(COMMITTED.packages, 'esbuild');
const lockedEsbuildLinux = entry(COMMITTED.packages, '@esbuild/linux-x64');
const CHART = `chart.js@${lockedChart.version}`;
const ESBUILD = `esbuild@${lockedEsbuild.version}`;
const ESBUILD_LINUX = `@esbuild/linux-x64@${lockedEsbuildLinux.version}`;

test('the committed lockfile passes against a registry that agrees with it', () => {
    const result = verify(COMMITTED);
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, /^\d+ locked packages pinned to the registry's own tarballs and integrity, and at least 14 days old$/m);
});

const refused = [
    // From the 0.22.0 security audit: each of these passed before.
    {
        name: 'a dot-dot path in resolved that fetches another package',
        change: (packages) => Object.assign(entry(packages, '@esbuild/linux-x64'), {
            resolved: `${REGISTRY}@esbuild/linux-x64/-/../../../evil-esbuild-linux-x64/-/evil-esbuild-linux-x64-1.0.0.tgz`,
            integrity: fakeIntegrity('A'),
        }),
        reasons: [
            `${ESBUILD_LINUX}: resolved URL "https://registry.npmjs.org/@esbuild/linux-x64/-/../../../evil-esbuild-linux-x64/-/evil-esbuild-linux-x64-1.0.0.tgz" is not in canonical form`,
            `${ESBUILD_LINUX}: resolved https://registry.npmjs.org/@esbuild/linux-x64/-/../../../evil-esbuild-linux-x64/-/evil-esbuild-linux-x64-1.0.0.tgz, but the registry lists ${lockedEsbuildLinux.resolved}`,
            `${ESBUILD_LINUX}: integrity differs from the registry's`,
        ],
    },
    {
        name: 'a percent-encoded dot-dot path in resolved',
        change: (packages) => {
            chart(packages).resolved = `${REGISTRY}chart.js/-/%2e%2e/%2E%2E/evil-chart/-/evil-chart-1.0.0.tgz`;
        },
        reasons: [`${CHART}: resolved URL`, 'is not in canonical form', `but the registry lists ${lockedChart.resolved}`],
    },
    {
        name: 'a version field older than the release resolved fetches',
        change: (packages) => {
            entry(packages, 'esbuild').resolved = tarballOf('esbuild', '0.29.9');
        },
        reasons: [`${ESBUILD}: resolved https://registry.npmjs.org/esbuild/-/esbuild-0.29.9.tgz, but the registry lists ${lockedEsbuild.resolved}`],
    },
    {
        name: "the fresh release's tarball and integrity under an old version",
        change: (packages) => Object.assign(entry(packages, 'esbuild'), {
            resolved: tarballOf('esbuild', '0.29.9'),
            integrity: fakeIntegrity('E'),
        }),
        reasons: [`${ESBUILD}: resolved https://registry.npmjs.org/esbuild/-/esbuild-0.29.9.tgz, but`, `${ESBUILD}: integrity differs from the registry's`],
    },
    // The other ways a tarball URL or hash can disagree with the registry.
    {
        name: "a sha512 integrity other than the registry's",
        change: (packages) => {
            chart(packages).integrity = fakeIntegrity('B');
        },
        reasons: [`${CHART}: integrity differs from the registry's`],
    },
    {
        name: 'a query string in resolved',
        change: (packages) => {
            chart(packages).resolved += '?evil=1';
        },
        reasons: [`${CHART}: resolved URL "${lockedChart.resolved}?evil=1" is not in canonical form`],
    },
    {
        name: 'an empty query string in resolved',
        change: (packages) => {
            chart(packages).resolved += '?';
        },
        reasons: ['is not in canonical form'],
    },
    {
        name: 'a fragment in resolved',
        change: (packages) => {
            chart(packages).resolved += '#evil';
        },
        reasons: [`${CHART}: resolved URL "${lockedChart.resolved}#evil" is not in canonical form`],
    },
    {
        name: 'backslashes in resolved',
        change: (packages) => {
            chart(packages).resolved = `${REGISTRY}chart.js/-/..\\..\\evil-chart\\-\\evil-chart-1.0.0.tgz`;
        },
        reasons: [`${CHART}: resolved URL`, 'is not in canonical form'],
    },
    {
        name: 'a tab in resolved',
        change: (packages) => {
            chart(packages).resolved = lockedChart.resolved.replace(/\.tgz$/, '\t.tgz');
        },
        reasons: ['is not in canonical form'],
    },
    {
        name: 'a release the registry does not have',
        change: (packages) => Object.assign(chart(packages), {
            version: '4.5.2',
            resolved: tarballOf('chart.js', '4.5.2'),
        }),
        reasons: ['chart.js@4.5.2: no such release in the registry', 'chart.js@4.5.2: no publish time in the registry'],
    },
    {
        name: 'a version that names an Object.prototype property',
        change: (packages) => {
            chart(packages).version = 'constructor';
        },
        reasons: ['chart.js@constructor: no such release in the registry'],
    },
    {
        name: 'an npm alias that installs another package',
        change: (packages) => {
            chart(packages).name = 'evil';
        },
        reasons: [`${CHART}: installs the package "evil"`],
    },
    {
        name: 'a link entry',
        change: (packages) => {
            chart(packages).link = true;
        },
        reasons: [`${CHART}: a link to ${lockedChart.resolved}, not a registry tarball`],
    },
    {
        name: 'a lockfile path that climbs out of node_modules',
        change: (packages) => {
            packages['node_modules/chart.js/../../evil'] = { ...chart(packages) };
        },
        reasons: ['node_modules/chart.js/../../evil: not a node_modules path of a registry package'],
    },
    {
        name: 'a lockfile path outside node_modules',
        change: (packages) => {
            packages['packages/evil'] = { ...chart(packages) };
        },
        reasons: ['packages/evil: not a node_modules path of a registry package'],
    },
    {
        name: 'a package the registry does not know',
        change: (packages) => {
            packages['node_modules/not-in-the-registry'] = {
                version: '1.0.0',
                resolved: tarballOf('not-in-the-registry', '1.0.0'),
                integrity: fakeIntegrity('N'),
            };
        },
        reasons: ['not-in-the-registry@1.0.0: registry answered 404'],
    },
    // Refused before the fix too; they must stay refused.
    {
        name: 'a git dependency',
        change: (packages) => {
            chart(packages).resolved = 'git+ssh://git@github.com/evil/chart.js.git#abc';
        },
        reasons: [`${CHART}: resolved from git+ssh://git@github.com/evil/chart.js.git#abc`],
    },
    {
        name: 'a file: link',
        change: (packages) => Object.assign(chart(packages), { resolved: 'file:../evil', link: true }),
        reasons: [`${CHART}: resolved from file:../evil`],
    },
    {
        name: 'a plain-http tarball',
        change: (packages) => {
            chart(packages).resolved = lockedChart.resolved.replace(/^https:/, 'http:');
        },
        reasons: [`${CHART}: resolved from ${lockedChart.resolved.replace(/^https:/, 'http:')}`],
    },
    {
        name: 'no resolved URL',
        change: (packages) => {
            delete chart(packages).resolved;
        },
        reasons: [`${CHART}: resolved from undefined`],
    },
    {
        name: 'a sha1 integrity',
        change: (packages) => {
            chart(packages).integrity = 'sha1-AAAA';
        },
        reasons: [`${CHART}: no sha512 integrity`],
    },
    {
        name: "another package's tarball under this name",
        change: (packages) => Object.assign(chart(packages), {
            name: 'evil',
            resolved: tarballOf('evil', '1.0.0'),
            integrity: fakeIntegrity('V'),
        }),
        reasons: [`${CHART}: resolved from https://registry.npmjs.org/evil/-/evil-1.0.0.tgz`],
    },
    {
        name: 'an honestly locked release under 14 days old',
        change: (packages) => Object.assign(entry(packages, 'esbuild'), {
            version: '0.29.9',
            resolved: tarballOf('esbuild', '0.29.9'),
            integrity: fakeIntegrity('E'),
        }),
        reasons: [`esbuild@0.29.9: published ${YESTERDAY.slice(0, 10)}, under 14 days ago`],
    },
    {
        name: 'a direct dependency that is not an exact pin',
        change: (packages) => {
            packages[''].dependencies['chart.js'] = '^4.5.1';
        },
        reasons: ['chart.js: "^4.5.1" is not an exact release version (x.y.z)'],
    },
    {
        name: 'a direct dependency that is not locked',
        change: (packages) => {
            packages[''].optionalDependencies = { evil: '1.0.0' };
        },
        reasons: ['evil: in optionalDependencies but not locked'],
    },
    {
        name: 'no locked packages',
        change: (packages) => {
            for (const path of Object.keys(packages)) if (path) delete packages[path];
        },
        reasons: ['no locked packages'],
    },
    {
        name: 'a lockfile v1 without a packages map',
        change: (packages, lock) => {
            lock.lockfileVersion = 1;
            delete lock.packages;
        },
        reasons: ['has no "packages" map with a root entry'],
    },
];

for (const { name, change, reasons } of refused) {
    test(`refuses ${name}`, () => {
        const result = verify(craft(change));
        assert.equal(result.status, 1, `exit status ${result.status}\n${result.stdout}${result.stderr}`);
        for (const reason of reasons) assert.ok(result.stderr.includes(reason), `missing "${reason}" in:\n${result.stderr}`);
    });
}
