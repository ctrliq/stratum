// The license check covers the npm packages Vite bundles into dash/dist. That
// set is not package.json's dependencies: react, react-dom and
// react-router-dom are devDependencies yet ship in the bundle, while vite and
// lightningcss are installed but only run the build. bundledPackages() turns
// the module ids the bundle was built from into the package list the check
// reads. Each test builds a throwaway node_modules tree under the OS temp
// directory, so the result depends on the fixture alone.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { bundledPackages } from '../bundled-packages.js';

// An empty directory standing in for dash/, removed when the test ends.
function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'bundled-packages-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

// Writes <dir>/package.json holding `manifest` and an empty file for each of
// `files`, and returns the files' absolute paths: the ids Rolldown hands to
// generateBundle for modules it bundled from that package.
function pkg(dir, manifest, files = ['index.js']) {
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, 'package.json'), JSON.stringify(manifest));
  return files.map((file) => {
    const id = path.join(dir, file);
    fs.mkdirSync(path.dirname(id), { recursive: true });
    fs.writeFileSync(id, '');
    return id;
  });
}

test('a package in the pnpm store is read from its own package.json', async (t) => {
  const root = fixture(t);
  const store = path.join(root, 'node_modules/.pnpm');
  const react = pkg(path.join(store, 'react@19.3.0/node_modules/react'),
    { name: 'react', version: '19.3.0', license: 'MIT' });
  // pnpm appends the peer set to the store directory's name; the package
  // itself sits under the inner node_modules.
  const reactDom = pkg(path.join(store, 'react-dom@19.3.0_react@19.3.0/node_modules/react-dom'),
    { name: 'react-dom', version: '19.3.0', license: 'MIT' }, ['cjs/react-dom-client.production.js']);
  // A nested package.json (nanoid ships these) only sets the module type; it
  // is not the package's manifest and must not stand in for it.
  const nanoidDir = path.join(store, 'nanoid@3.3.18/node_modules/nanoid');
  pkg(nanoidDir, { name: 'nanoid', version: '3.3.18', license: 'MIT' });
  const nanoid = pkg(path.join(nanoidDir, 'non-secure'), { type: 'module', main: 'index.cjs' });

  assert.deepEqual(await bundledPackages([...react, ...reactDom, ...nanoid]), [
    { name: 'nanoid', version: '3.3.18', license: 'MIT' },
    { name: 'react', version: '19.3.0', license: 'MIT' },
    { name: 'react-dom', version: '19.3.0', license: 'MIT' },
  ]);
});

test('a scoped package keeps its scope, in the pnpm store and in a flat node_modules', async (t) => {
  const root = fixture(t);
  // pnpm writes @xterm/xterm's store directory as @xterm+xterm@<version>.
  const xterm = pkg(path.join(root, 'node_modules/.pnpm/@xterm+xterm@5.5.0/node_modules/@xterm/xterm'),
    { name: '@xterm/xterm', version: '5.5.0', license: 'MIT' }, ['lib/xterm.mjs']);
  const fit = pkg(path.join(root, 'node_modules/@xterm/addon-fit'),
    { name: '@xterm/addon-fit', version: '0.11.0', license: 'MIT' }, ['lib/addon-fit.mjs']);

  assert.deepEqual(await bundledPackages([...xterm, ...fit]), [
    { name: '@xterm/addon-fit', version: '0.11.0', license: 'MIT' },
    { name: '@xterm/xterm', version: '5.5.0', license: 'MIT' },
  ]);
});

test('virtual modules and the app\'s own sources are not packages', async (t) => {
  const root = fixture(t);
  // The app's own manifest sits above its sources; reading it would list the
  // dashboard as a dependency of itself.
  const [app] = pkg(root, { name: '@ctrliq/stratum-dash', version: '0.0.0', license: 'UNLICENSED' }, ['src/app.jsx']);
  const html = path.join(root, 'index.html');
  fs.writeFileSync(html, '');
  const cookie = pkg(path.join(root, 'node_modules/.pnpm/cookie@1.1.1/node_modules/cookie'),
    { name: 'cookie', version: '1.1.1', license: 'MIT' }, ['dist/index.js']);
  // A virtual id names a node_modules path but is a plugin's wrapper, not the
  // file; it starts with \0 and must be skipped before any path is read.
  const [wrapped] = pkg(path.join(root, 'node_modules/.pnpm/virt@1.0.0/node_modules/virt'),
    { name: 'virt', version: '1.0.0', license: 'GPL-3.0-only' });

  const ids = ['\0vite/preload-helper.js', '\0commonjsHelpers.js', `\0${wrapped}?commonjs-proxy`, app, html, ...cookie];
  assert.deepEqual(await bundledPackages(ids), [
    { name: 'cookie', version: '1.1.1', license: 'MIT' },
  ]);
});

test('no package modules give an empty list', async () => {
  assert.deepEqual(await bundledPackages([]), []);
  assert.deepEqual(await bundledPackages(['\0vite/modulepreload-polyfill.js']), []);
});

test('every module of a package, and every copy of one version, give one entry', async (t) => {
  const root = fixture(t);
  const store = path.join(root, 'node_modules/.pnpm');
  const router = { name: 'react-router', version: '7.18.4', license: 'MIT' };
  // The same version installed twice under different peer sets, and reached
  // once more through a flat node_modules: still one package to license.
  const a = pkg(path.join(store, 'react-router@7.18.4_react-dom@19.3.0_react@19.3.0__react@19.3.0/node_modules/react-router'),
    router, ['dist/production/index.mjs', 'dist/production/chunk-a.mjs', 'dist/production/chunk-b.mjs']);
  const b = pkg(path.join(store, 'react-router@7.18.4_react@19.3.0/node_modules/react-router'),
    router, ['dist/production/index.mjs']);
  const c = pkg(path.join(root, 'node_modules/react-router'), router, ['dist/production/index.mjs']);
  // Two versions of one name are two packages, each with its own license.
  const old = pkg(path.join(store, 'scheduler@0.27.0/node_modules/scheduler'),
    { name: 'scheduler', version: '0.27.0', license: 'MIT' });
  const cur = pkg(path.join(store, 'scheduler@0.28.0/node_modules/scheduler'),
    { name: 'scheduler', version: '0.28.0', license: 'MIT' });

  assert.deepEqual(await bundledPackages([...a, ...b, ...c, ...a, ...cur, ...old, ...cur]), [
    router,
    { name: 'scheduler', version: '0.27.0', license: 'MIT' },
    { name: 'scheduler', version: '0.28.0', license: 'MIT' },
  ]);
});

test('copies of one version that declare different licenses each keep their entry', async (t) => {
  const root = fixture(t);
  const store = path.join(root, 'node_modules/.pnpm');
  const mit = pkg(path.join(store, 'same@1.0.0_a@1.0.0/node_modules/same'), { name: 'same', version: '1.0.0', license: 'MIT' });
  const gpl = pkg(path.join(store, 'same@1.0.0_b@1.0.0/node_modules/same'), { name: 'same', version: '1.0.0', license: 'GPL-3.0-only' });

  assert.deepEqual(await bundledPackages([...mit, ...gpl]), [
    { name: 'same', version: '1.0.0', license: 'GPL-3.0-only' },
    { name: 'same', version: '1.0.0', license: 'MIT' },
  ]);
});

test('entries come out sorted whatever order the modules arrive in', async (t) => {
  const root = fixture(t);
  const store = path.join(root, 'node_modules/.pnpm');
  const ids = [
    ...pkg(path.join(store, 'scheduler@0.28.0/node_modules/scheduler'), { name: 'scheduler', version: '0.28.0', license: 'MIT' }),
    ...pkg(path.join(store, 'scheduler@0.27.0/node_modules/scheduler'), { name: 'scheduler', version: '0.27.0', license: 'MIT' }),
    ...pkg(path.join(store, 'react-dom@19.3.0_react@19.3.0/node_modules/react-dom'), { name: 'react-dom', version: '19.3.0', license: 'MIT' }),
    ...pkg(path.join(store, 'react@19.3.0/node_modules/react'), { name: 'react', version: '19.3.0', license: 'MIT' }),
    ...pkg(path.join(store, 'cookie@1.1.1/node_modules/cookie'), { name: 'cookie', version: '1.1.1', license: 'MIT' }),
    ...pkg(path.join(store, '@xterm+xterm@5.5.0/node_modules/@xterm/xterm'), { name: '@xterm/xterm', version: '5.5.0', license: 'MIT' }),
  ];
  const want = [
    { name: '@xterm/xterm', version: '5.5.0', license: 'MIT' },
    { name: 'cookie', version: '1.1.1', license: 'MIT' },
    { name: 'react', version: '19.3.0', license: 'MIT' },
    { name: 'react-dom', version: '19.3.0', license: 'MIT' },
    { name: 'scheduler', version: '0.27.0', license: 'MIT' },
    { name: 'scheduler', version: '0.28.0', license: 'MIT' },
  ];
  const given = [...ids];
  assert.deepEqual(await bundledPackages(ids), want);
  // The function is pure: sorting must not reorder the caller's array.
  assert.deepEqual(ids, given);
  assert.deepEqual(await bundledPackages([...ids].reverse()), want);
});

test('a missing or non-string license is recorded as ""', async (t) => {
  const root = fixture(t);
  const store = path.join(root, 'node_modules/.pnpm');
  const ids = [
    ...pkg(path.join(store, 'bare@1.0.0/node_modules/bare'), { name: 'bare', version: '1.0.0' }),
    // The deprecated object and array forms carry a type, but not as the
    // string the check classifies; the check warns on "" rather than guess.
    ...pkg(path.join(store, 'legacy-object@1.0.0/node_modules/legacy-object'),
      { name: 'legacy-object', version: '1.0.0', license: { type: 'MIT', url: 'https://example.invalid/LICENSE' } }),
    ...pkg(path.join(store, 'legacy-array@1.0.0/node_modules/legacy-array'),
      { name: 'legacy-array', version: '1.0.0', licenses: [{ type: 'MIT' }] }),
    ...pkg(path.join(store, 'nulled@1.0.0/node_modules/nulled'), { name: 'nulled', version: '1.0.0', license: null }),
    // A string license, SPDX expression included, is recorded verbatim.
    ...pkg(path.join(store, 'dual@1.0.0/node_modules/dual'), { name: 'dual', version: '1.0.0', license: '(MIT OR Apache-2.0)' }),
  ];

  assert.deepEqual(await bundledPackages(ids), [
    { name: 'bare', version: '1.0.0', license: '' },
    { name: 'dual', version: '1.0.0', license: '(MIT OR Apache-2.0)' },
    { name: 'legacy-array', version: '1.0.0', license: '' },
    { name: 'legacy-object', version: '1.0.0', license: '' },
    { name: 'nulled', version: '1.0.0', license: '' },
  ]);
});

test('a package whose manifest cannot be read fails the call instead of dropping out', async (t) => {
  const root = fixture(t);
  const store = path.join(root, 'node_modules/.pnpm');
  // Skipping the package would leave it out of the list, and the license
  // check would pass without ever seeing it; the readable one beside it must
  // not turn the call into a partial answer.
  const cookie = pkg(path.join(store, 'cookie@1.1.1/node_modules/cookie'),
    { name: 'cookie', version: '1.1.1', license: 'MIT' }, ['dist/index.js']);

  const missing = path.join(store, 'gone@1.0.0/node_modules/gone/index.js');
  fs.mkdirSync(path.dirname(missing), { recursive: true });
  fs.writeFileSync(missing, '');
  await assert.rejects(bundledPackages([...cookie, missing]), /cannot read/);

  const [invalid] = pkg(path.join(store, 'torn@1.0.0/node_modules/torn'), {});
  fs.writeFileSync(path.join(path.dirname(invalid), 'package.json'), '{"name": "torn",');
  await assert.rejects(bundledPackages([...cookie, invalid]), /cannot read/);
});
