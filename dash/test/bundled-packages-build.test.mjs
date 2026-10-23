// bundledPackagesPlugin() must list every npm package whose code reaches
// dash/dist, whichever way Vite pulled it in: a JS import, a lazy chunk, a
// web worker (a build of its own), a file a stylesheet's url() names (an
// asset, never a module) or a small file Vite inlined as a data: URL (neither
// a module nor an asset). A stylesheet @import inlines the file it names
// without making it a module, so the plugin fails the build instead of
// letting a package in unseen. Each test runs a real Vite build of a
// throwaway app under the OS temp directory, so the result depends on the
// fixture alone and dash/dist and dash/.bundle are never touched.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { build, createBuilder } from 'vite';
import { bundledPackagesPlugin } from '../bundled-packages.js';

const HTML = '<!doctype html><script type="module" src="/src/main.js"></script>\n';

// Writes `files` (path relative to the app → contents) into a fresh directory,
// removed when the test ends, and returns it. The directory is resolved to its
// real path so that it matches the module ids Vite reports.
function app(t, files) {
  const root = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'bundled-packages-build-')));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  for (const [name, contents] of Object.entries(files)) {
    const file = path.join(root, name);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, contents);
  }
  return root;
}

// The files of an installed package `name`: its manifest and `files`.
function pkg(name, license, files) {
  const dir = `node_modules/${name}`;
  return {
    [`${dir}/package.json`]: JSON.stringify({ name, version: '1.0.0', license }),
    ...Object.fromEntries(Object.entries(files).map(([file, contents]) => [`${dir}/${file}`, contents])),
  };
}

// The config that builds the app at `root` with the plugin, as vite.config.js
// uses it, then `plugins` after it, plus `config` (its `build` options merged
// into the test's own).
function viteConfig(root, { build: options, plugins = [], ...config } = {}) {
  return {
    root,
    configFile: false,
    logLevel: 'silent',
    plugins: [bundledPackagesPlugin(), ...plugins],
    build: { outDir: path.join(root, 'dist'), emptyOutDir: true, ...options },
    ...config,
  };
}

// The package list the plugin wrote for the app at `root`.
function packages(root) {
  return JSON.parse(fs.readFileSync(path.join(root, '.bundle', 'packages.json'), 'utf8'));
}

// Builds the app at `root` with viteConfig(root, config) and returns the
// package list the plugin wrote.
async function bundle(root, config) {
  await build(viteConfig(root, config));
  return packages(root);
}

// Whether `contents` ships inside a file of the app's dist as a base64 data:
// URL, which is how Vite inlines a file that is not an SVG.
function inlined(root, contents) {
  const encoded = Buffer.from(contents).toString('base64');
  return fs.readdirSync(path.join(root, 'dist'), { recursive: true })
    .map((name) => path.join(root, 'dist', name))
    .some((file) => fs.statSync(file).isFile() && fs.readFileSync(file, 'utf8').includes(encoded));
}

test('every package whose code reaches dist is listed', async (t) => {
  const root = app(t, {
    'index.html': HTML,
    'src/main.js': [
      "import { js } from 'js-pkg';",
      "import 'style-pkg/style.css';",
      "import './app.css';",
      'console.log(js());',
      "import('lazy-pkg').then((m) => console.log(m.lazy()));",
      "new Worker(new URL('./worker.js', import.meta.url), { type: 'module' });",
      '',
    ].join('\n'),
    // The worker is bundled by a build of its own; the main build sees only
    // the file it wrote, with no modules and no source file.
    'src/worker.js': "import { work } from 'worker-pkg';\npostMessage(work());\n",
    // An @import of the app's own stylesheet is fine: a bare-looking name
    // resolves beside the stylesheet first, as Vite resolves it. The font is
    // an asset, which reaches dist without being a module of any chunk.
    'src/app.css': "@import 'partial.css';\n@font-face { font-family: F; src: url('font-pkg/font.woff2'); }\n",
    'src/partial.css': '.partial { color: red; }\n',
    ...pkg('js-pkg', 'MIT', { 'index.js': "export const js = () => 'js';\n" }),
    ...pkg('lazy-pkg', 'ISC', { 'index.js': "export const lazy = () => 'lazy';\n" }),
    ...pkg('worker-pkg', 'Apache-2.0', { 'index.js': "export const work = () => 'work';\n" }),
    // A stylesheet imported from JS is a module; its @import of a file in its
    // own package inlines nothing from any other package.
    ...pkg('style-pkg', 'BSD-3-Clause', {
      'style.css': "@import './base.css';\n.style { color: blue; }\n",
      'base.css': '.base { color: green; }\n',
    }),
    // Over Vite's 4 KiB assetsInlineLimit, so it is written to dist as a file.
    ...pkg('font-pkg', 'OFL-1.1', { 'font.woff2': 'x'.repeat(8192) }),
  });

  assert.deepEqual(await bundle(root), [
    { name: 'font-pkg', version: '1.0.0', license: 'OFL-1.1' },
    { name: 'js-pkg', version: '1.0.0', license: 'MIT' },
    { name: 'lazy-pkg', version: '1.0.0', license: 'ISC' },
    { name: 'style-pkg', version: '1.0.0', license: 'BSD-3-Clause' },
    { name: 'worker-pkg', version: '1.0.0', license: 'Apache-2.0' },
  ]);
});

test('a package whose file Vite inlines as a data: URL is listed', async (t) => {
  // Under Vite's 4 KiB assetsInlineLimit, so it ships inside a chunk or a
  // stylesheet, with no module and no asset of its own.
  const icon = 'icon'.repeat(25);
  const iconPkg = pkg('icon-pkg', 'GPL-3.0-only', { 'icon.png': icon });
  const worker = "new Worker(new URL('./worker.js', import.meta.url), { type: 'module' });\n";
  const cases = {
    'from a stylesheet url()': {
      'src/main.js': "import './app.css';\n",
      'src/app.css': ".icon { background: url('icon-pkg/icon.png'); }\n",
    },
    // postcss-import inlines the partial first; Vite resolves its url()
    // against the partial's own directory.
    'from an @imported stylesheet\'s url()': {
      'src/main.js': "import './app.css';\n",
      'src/app.css': "@import './partial.css';\n",
      'src/partial.css': '.icon { background: url(../node_modules/icon-pkg/icon.png); }\n',
    },
    // image-set() takes a bare string as well as a url().
    'from image-set()': {
      'src/main.js': "import './app.css';\n",
      'src/app.css': '.icon { background: image-set("icon-pkg/icon.png" 1x); }\n',
    },
    'from a worker\'s stylesheet': {
      'src/main.js': worker,
      'src/worker.js': "import './worker.css';\npostMessage('ready');\n",
      'src/worker.css': ".icon { background: url('icon-pkg/icon.png'); }\n",
    },
    'from new URL()': {
      'src/main.js': "console.log(new URL('icon-pkg/icon.png', import.meta.url).href);\n",
    },
    'from new URL() in a worker': {
      'src/main.js': worker,
      'src/worker.js': "postMessage(new URL('icon-pkg/icon.png', import.meta.url).href);\n",
    },
    'from index.html': {
      'index.html': '<!doctype html><img src="/node_modules/icon-pkg/icon.png"><script type="module" src="/src/main.js"></script>\n',
      'src/main.js': "console.log('main');\n",
    },
  };
  for (const [name, files] of Object.entries(cases)) {
    await t.test(name, async (t) => {
      const root = app(t, { 'index.html': HTML, ...iconPkg, ...files });

      assert.deepEqual(await bundle(root), [{ name: 'icon-pkg', version: '1.0.0', license: 'GPL-3.0-only' }]);
      assert.ok(inlined(root, icon), 'icon.png was not inlined, so this case proves nothing about inlined files');
    });
  }
});

test('the configured assetsInlineLimit still decides what Vite inlines', async (t) => {
  // Vite reads an undefined answer as its 4 KiB default, so each case uses a
  // file that default would treat the other way: only the configured limit
  // can decide it as the case expects.
  const small = 'font'.repeat(25);
  const large = 'font'.repeat(2048);
  // An app whose stylesheet names font-pkg's font, holding `font`.
  const fontApp = (t, font) => app(t, {
    'index.html': HTML,
    'src/main.js': "import './app.css';\n",
    'src/app.css': "@font-face { font-family: F; src: url('font-pkg/font.woff2'); }\n",
    ...pkg('font-pkg', 'OFL-1.1', { 'font.woff2': font }),
  });
  const listed = [{ name: 'font-pkg', version: '1.0.0', license: 'OFL-1.1' }];

  // vite.config.js sets no limit, so this is the dashboard's own build.
  await t.test('with no limit configured, Vite\'s 4 KiB default decides', async (t) => {
    const root = fontApp(t, large);
    assert.deepEqual(await bundle(root), listed);
    assert.ok(!inlined(root, large));
  });
  // Vite inlines only a file smaller than the limit, so one of exactly the
  // limit's size is written out.
  await t.test('a number no larger than the file\'s size writes it to dist as a file', async (t) => {
    const root = fontApp(t, small);
    assert.deepEqual(await bundle(root, { build: { assetsInlineLimit: small.length } }), listed);
    assert.ok(!inlined(root, small));
    assert.ok(fs.readdirSync(path.join(root, 'dist/assets')).some((name) => name.endsWith('.woff2')));
  });
  await t.test('a number above the file\'s size inlines it', async (t) => {
    const root = fontApp(t, large);
    assert.deepEqual(await bundle(root, { build: { assetsInlineLimit: 16384 } }), listed);
    assert.ok(inlined(root, large));
  });
  await t.test('a function is asked about the file and its answer holds', async (t) => {
    const root = fontApp(t, large);
    const asked = [];
    const assetsInlineLimit = (file) => {
      asked.push(file);
      return true;
    };
    assert.deepEqual(await bundle(root, { build: { assetsInlineLimit } }), listed);
    assert.ok(inlined(root, large));
    assert.deepEqual(asked, [path.join(root, 'node_modules/font-pkg/font.woff2')]);
  });
  await t.test('a function is given the file\'s contents and its false holds', async (t) => {
    const root = fontApp(t, small);
    const assetsInlineLimit = (_file, content) => content.length > 1000;
    assert.deepEqual(await bundle(root, { build: { assetsInlineLimit } }), listed);
    assert.ok(!inlined(root, small));
  });
  await t.test('a function that answers undefined leaves the file to Vite\'s default', async (t) => {
    const root = fontApp(t, small);
    assert.deepEqual(await bundle(root, { build: { assetsInlineLimit: () => undefined } }), listed);
    assert.ok(inlined(root, small));
  });
});

test('a build that would inline files unseen fails', async (t) => {
  const cases = {
    'when a later plugin replaces assetsInlineLimit': {
      plugins: [{ name: 'later', config: () => ({ build: { assetsInlineLimit: 0 } }) }],
      error: /the client environment's build\.assetsInlineLimit is 0, not the wrapper/,
    },
    // After every config hook has run, so only the check at buildStart sees it.
    'when a plugin replaces it in the resolved config': {
      plugins: [{
        name: 'resolved',
        configResolved(config) {
          config.environments.client.build.assetsInlineLimit = () => true;
        },
      }],
      error: /the client environment's build\.assetsInlineLimit is another function, not the wrapper/,
    },
    'when an environment sets its own assetsInlineLimit': {
      environments: { client: { build: { assetsInlineLimit: 0 } } },
      error: /the client environment's build\.assetsInlineLimit is 0, not the wrapper/,
    },
    'when it is a library build': {
      build: { lib: { entry: 'src/main.js', formats: ['es'] } },
      error: /the client environment's build\.lib is set/,
    },
  };
  for (const [name, { error, ...config }] of Object.entries(cases)) {
    await t.test(name, async (t) => {
      const root = app(t, { 'index.html': HTML, 'src/main.js': "console.log('main');\n" });
      await assert.rejects(bundle(root, config), error);
    });
  }
});

// Vite's builder mode (builder: {} in the config, or vite build --app)
// resolves the config again for each environment, so the plugin's config hook
// runs twice and the second time finds its own wrapper as the limit.
test('a builder-mode build lists the package whose file Vite inlines', async (t) => {
  const icon = 'icon'.repeat(25);
  const root = app(t, {
    'index.html': HTML,
    'src/main.js': "import './app.css';\n",
    'src/app.css': ".icon { background: url('icon-pkg/icon.png'); }\n",
    ...pkg('icon-pkg', 'GPL-3.0-only', { 'icon.png': icon }),
  });

  await (await createBuilder(viteConfig(root, { builder: {} }))).buildApp();
  assert.deepEqual(packages(root), [{ name: 'icon-pkg', version: '1.0.0', license: 'GPL-3.0-only' }]);
  assert.ok(inlined(root, icon));
});

// Vite keeps build.rollupOptions as an alias of build.rolldownOptions; a
// plugin that copied build into a new object would turn the alias into a
// plain value, and a later plugin's options set through it would be lost.
test('a later plugin\'s build.rollupOptions still take effect', async (t) => {
  const root = app(t, { 'index.html': HTML, 'src/main.js': "console.log('main');\n" });
  const later = {
    name: 'later',
    config(config) {
      config.build.rollupOptions = { output: { entryFileNames: 'later-[name].js' } };
    },
  };

  await bundle(root, { build: { rollupOptions: { output: { entryFileNames: 'user-[name].js' } } }, plugins: [later] });
  assert.deepEqual(fs.readdirSync(path.join(root, 'dist')).filter((name) => name.endsWith('.js')), ['later-index.js']);
});

test('worker plugins from the config still run beside the plugin\'s own', async (t) => {
  const files = {
    'index.html': HTML,
    'src/main.js': "new Worker(new URL('./worker.js', import.meta.url), { type: 'module' });\n",
    'src/worker.js': "import { work } from 'worker-pkg';\npostMessage(work());\n",
    ...pkg('worker-pkg', 'Apache-2.0', { 'index.js': "export const work = () => 'work';\n" }),
  };
  // worker.plugins is a function; an array is its deprecated form, which
  // Vite still accepts.
  const forms = { 'as a function': (user) => () => [user], 'as an array': (user) => [user] };
  for (const [name, form] of Object.entries(forms)) {
    await t.test(name, async (t) => {
      const root = app(t, files);
      const seen = [];
      const user = { name: 'user-worker', transform(_code, id) { seen.push(id); } };

      assert.deepEqual(await bundle(root, { worker: { plugins: form(user) } }), [
        { name: 'worker-pkg', version: '1.0.0', license: 'Apache-2.0' },
      ]);
      assert.ok(seen.includes(path.join(root, 'src/worker.js')), `user worker plugin saw ${JSON.stringify(seen)}`);
    });
  }
});

test('a ?inline reference fails the build', async (t) => {
  const font = pkg('font-pkg', 'OFL-1.1', { 'font.woff2': 'font' });
  const cases = {
    'from a stylesheet\'s url()': {
      'src/main.js': "import './app.css';\n",
      'src/app.css': "@font-face { font-family: F; src: url('font-pkg/font.woff2?inline'); }\n",
    },
    'as a later query parameter': {
      'src/main.js': "import './app.css';\n",
      'src/app.css': "@font-face { font-family: F; src: url('font-pkg/font.woff2?v=1&inline'); }\n",
    },
    'from an @imported stylesheet': {
      'src/main.js': "import './app.css';\n",
      'src/app.css': "@import './partial.css';\n",
      'src/partial.css': "@font-face { font-family: F; src: url('font-pkg/font.woff2?inline'); }\n",
    },
    'from new URL()': {
      'src/main.js': "console.log(new URL('font-pkg/font.woff2?inline', import.meta.url).href);\n",
    },
    'from a package': {
      'src/main.js': "import 'style-pkg/style.css';\n",
      ...pkg('style-pkg', 'MIT', { 'style.css': "@font-face { font-family: F; src: url('font-pkg/font.woff2?inline'); }\n" }),
    },
    'from a worker': {
      'src/main.js': "new Worker(new URL('./worker.js', import.meta.url), { type: 'module' });\n",
      'src/worker.js': "postMessage(new URL('font-pkg/font.woff2?inline', import.meta.url).href);\n",
    },
  };
  // The build transforms index.html as a module, so the same check covers it.
  await t.test('from index.html', async (t) => {
    const root = app(t, {
      ...font,
      'index.html': `<link rel="icon" href="./node_modules/font-pkg/font.woff2?inline">${HTML}`,
      'src/main.js': "console.log('ready');\n",
    });
    await assert.rejects(bundle(root), /bundled packages: index\.html writes a \?inline query/);
  });
  for (const [name, files] of Object.entries(cases)) {
    await t.test(name, async (t) => {
      const root = app(t, { 'index.html': HTML, ...font, ...files });
      await assert.rejects(bundle(root), /bundled packages: .* writes a \?inline query/);
    });
  }
});

test('a stylesheet that @imports a package fails the build', async (t) => {
  const css = pkg('css-pkg', 'GPL-3.0-only', { 'style.css': '.css-pkg { color: red; }\n' });
  const cases = {
    // A bare specifier: Vite finds it in node_modules.
    'from the app': {
      'src/main.js': "import './app.css';\n",
      'src/app.css': "@import 'css-pkg/style.css';\n",
    },
    // CSS at-rule names are case-insensitive, so this is an @import too.
    'with an uppercase at-rule name': {
      'src/main.js': "import './app.css';\n",
      'src/app.css': "@IMPORT 'css-pkg/style.css';\n",
    },
    // A comment, or nothing at all, may separate the at-rule from its URL.
    'with a comment after the at-rule name': {
      'src/main.js': "import './app.css';\n",
      'src/app.css': '@import/**/"css-pkg/style.css";\n',
    },
    'with no space after the at-rule name': {
      'src/main.js': "import './app.css';\n",
      'src/app.css': '@import"css-pkg/style.css";\n',
    },
    // A path into node_modules, from a stylesheet that is itself @imported
    // and so is never a module the plugin is shown.
    'through an @imported stylesheet': {
      'src/main.js': "import './app.css';\n",
      'src/app.css': "@import './partial.css';\n",
      'src/partial.css': '@import url("../node_modules/css-pkg/style.css") layer(vendor);\n',
    },
    // The importing package is listed; the one it @imports would not be.
    'from another package': {
      'src/main.js': "import 'style-pkg/style.css';\n",
      ...pkg('style-pkg', 'MIT', { 'style.css': "@import 'css-pkg/style.css';\n" }),
    },
    // A worker's stylesheet ships inside the worker's script.
    'from a worker': {
      'src/main.js': "new Worker(new URL('./worker.js', import.meta.url), { type: 'module' });\n",
      'src/worker.js': "import './worker.css';\npostMessage('ready');\n",
      'src/worker.css': "@import 'css-pkg/style.css';\n",
    },
  };
  for (const [name, files] of Object.entries(cases)) {
    await t.test(name, async (t) => {
      const root = app(t, { 'index.html': HTML, ...css, ...files });
      await assert.rejects(bundle(root), /bundled packages: .*@imports .*css-pkg\/style\.css/);
    });
  }
});
