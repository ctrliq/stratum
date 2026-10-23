// The license check covers the npm packages Vite bundles into dash/dist. That
// set is neither package.json's dependencies (react, react-dom and
// react-router-dom are devDependencies yet ship in the bundle) nor everything
// installed (vite, rolldown and lightningcss only run the build). Only the
// bundle knows which modules it holds, so the plugin below asks it at
// generateBundle and records their packages for the license check to read.
import fs from 'node:fs/promises';
import path from 'node:path';

// The directory and name of the package the module `id` was bundled from, or
// null when no package holds it. A virtual id (a plugin's \0-prefixed wrapper,
// which can name the node_modules file it wraps) is not that file, and an id
// outside node_modules is the app's own source. Ids are POSIX paths, which Vite
// guarantees on every platform. The last node_modules segment is the one that
// matters: pnpm keeps a package at
// node_modules/.pnpm/<name>@<version>[_<peers>]/node_modules/<name>/, and a
// scoped name spans two segments (@scope/name).
function packageOf(id) {
  if (id.startsWith('\0')) return null;
  const parts = id.replace(/\?.*$/, '').split('/');
  const at = parts.lastIndexOf('node_modules');
  if (at < 0) return null;
  const end = at + (parts[at + 1]?.startsWith('@') ? 3 : 2);
  // A module is a file inside its package's directory; anything shorter is a
  // file sitting directly in node_modules, which no package owns.
  if (end >= parts.length) return null;
  return { dir: parts.slice(0, end).join('/'), name: parts.slice(at + 1, end).join('/') };
}

// The package's own manifest at the top of its directory. A package.json
// deeper in the tree (nanoid/non-secure/package.json) only sets a module type
// and never stands in for it. A manifest that cannot be read is an error, not
// a skip: a dropped package would pass the license check silently. A manifest
// without a name falls back to the name its directory was installed under.
async function readPackage({ dir, name }) {
  const file = path.join(dir, 'package.json');
  let manifest;
  try {
    manifest = JSON.parse(await fs.readFile(file, 'utf8'));
  } catch (err) {
    throw new Error(`bundled packages: cannot read ${file}: ${err.message}`, { cause: err });
  }
  return {
    name: typeof manifest.name === 'string' && manifest.name !== '' ? manifest.name : name,
    version: typeof manifest.version === 'string' ? manifest.version : '',
    // The deprecated object ({type, url}) and array (licenses: [...]) forms are
    // not the SPDX string the check classifies; "" makes it warn, not guess.
    license: typeof manifest.license === 'string' ? manifest.license : '',
  };
}

function byNameThenVersion(a, b) {
  if (a.name !== b.name) return a.name < b.name ? -1 : 1;
  if (a.version !== b.version) return a.version < b.version ? -1 : 1;
  if (a.license !== b.license) return a.license < b.license ? -1 : 1;
  return 0;
}

// bundledPackages maps the module ids of a bundle to its npm packages: one
// {name, version, license} entry per name, version and license, however many
// modules or installed copies (pnpm keeps one per peer set) it was bundled
// from, sorted by name, then version, then license. Copies of one version
// that declare different licenses each keep their entry, so the check judges
// every license that ships. It never modifies `moduleIds`.
export async function bundledPackages(moduleIds) {
  const dirs = new Map();
  for (const id of moduleIds) {
    const found = packageOf(id);
    if (found !== null) dirs.set(found.dir, found);
  }
  const packages = new Map();
  for (const pkg of await Promise.all([...dirs.values()].map(readPackage))) {
    const key = `${pkg.name}\0${pkg.version}\0${pkg.license}`;
    if (!packages.has(key)) packages.set(key, pkg);
  }
  return [...packages.values()].sort(byNameThenVersion);
}

// The ids of the files whose code a bundle ships. A chunk's moduleIds hold
// only the modules it kept, so a package the build resolved but tree-shook
// away entirely (react-router-dom's re-exports, react-router's server-only
// cookie dependencies) is not among them. An asset (a font or image a
// stylesheet's url() names) is no chunk's module: Vite records the file it
// copied in originalFileNames, relative to the root or absolute, which is
// resolved here to an id of the same POSIX form as a module's. A file Vite
// inlined as a data: URL is neither; bundledPackagesPlugin records those.
function bundleIds(bundle, root) {
  return Object.values(bundle).flatMap((output) => (output.type === 'chunk'
    ? output.moduleIds
    : output.originalFileNames.map((name) => path.resolve(root, name).split(path.sep).join('/'))));
}

// The stylesheets checkImports reads: plain CSS, which Vite compiles with
// PostCSS. Vite's CSS plugin skips the special queries (a ?raw stylesheet is
// a string, never compiled), and a \0 id is a plugin's virtual module, not a
// file. Sass, Less and Stylus resolve their own imports, which checkImports
// does not follow; the dashboard has none of them.
const STYLESHEET = /\.(?:css|pcss|postcss)(?:$|\?)/;
const NOT_COMPILED = /^\0|[?&](?:worker|sharedworker|raw|url|commonjs-proxy)\b/;
// Each comment is read as a space, as CSS reads it, so @import/**/"x" still
// matches IMPORT_RULE.
const COMMENT = /\/\*[\s\S]*?\*\//g;
// The target of an @import rule: quoted, or in url() with or without quotes.
// CSS at-rule and function names are case-insensitive, so @IMPORT is one too.
const IMPORT_RULE = /@import\s*(?:url\(\s*)?(?:"([^"]*)"|'([^']*)'|([^\s"'();]+))/gi;
const HAS_IMPORT = /@import/i;
// An inline query parameter (?inline, or &inline after another) makes Vite
// inline the file it names without asking assetsInlineLimit, so the file
// would ship unseen.
const INLINE_QUERY = /[?&]inline\b/;
// A target with a scheme (https:, data:) or a protocol-relative one is left
// for the browser to fetch; nothing of it is bundled.
const REMOTE = /^(?:[a-z][a-z\d+.-]*:|\/\/)/i;

// The file an @import `target` in the stylesheet `file` names, found the way
// Vite's CSS resolver finds it (preferRelative, extension .css): beside the
// stylesheet, a bare name included, or for /path under the root and then on
// disk. null when it is none of these, which leaves only a package (or an
// alias, which the dashboard does not use).
async function importedFile(target, file, root) {
  const name = target.replace(/[?#].*$/, '');
  const bases = name.startsWith('/') ? [path.join(root, name), name] : [path.resolve(path.dirname(file), name)];
  for (const base of bases) {
    for (const candidate of [base, `${base}.css`]) {
      if ((await fs.stat(candidate).catch(() => null))?.isFile()) return candidate;
    }
  }
  return null;
}

// refuseInline fails the build when code writes an inline query.
function refuseInline(file, code, root) {
  if (INLINE_QUERY.test(code)) {
    throw new Error(`bundled packages: ${path.relative(root, file)} writes a ?inline query, which makes Vite `
      + 'inline the file it names unseen by the license check; reference the file without it');
  }
}

// Vite inlines the file a stylesheet @imports into the stylesheet without
// making it a module, so no chunk's moduleIds name it. The CSS plugin does
// register it with this.addWatchFile, but a Rolldown plugin context has no
// getWatchFiles to read that back. A package's stylesheet reached that way
// would ship unlisted, so checkImports fails the build instead: an @import
// must name a file of the app, or of the package `file` belongs to. It reads
// on into every file it allows, whose own @imports are never modules either.
// The app takes a package's stylesheet by importing it from JS instead, which
// makes it a module the plugin lists.
async function checkImports(file, code, root, seen) {
  // An @imported stylesheet is never a module, so transform's own check
  // never reads it.
  refuseInline(file, code, root);
  const owner = packageOf(file)?.dir;
  for (const match of code.replace(COMMENT, ' ').matchAll(IMPORT_RULE)) {
    const target = match[1] ?? match[2] ?? match[3];
    if (REMOTE.test(target)) continue;
    const found = await importedFile(target, file, root);
    const into = found === null ? null : packageOf(found);
    if (found === null || (into !== null && into.dir !== owner)) {
      const what = found === null ? 'not a file beside it or under the root' : `a file of ${into.name}`;
      throw new Error(`bundled packages: ${path.relative(root, file)} @imports "${target}", which is ${what}; `
        + 'Vite inlines an @imported stylesheet unseen by the license check, so import a package\'s stylesheet '
        + 'from JS instead');
    }
    if (seen.has(found)) continue;
    seen.add(found);
    await checkImports(found, await fs.readFile(found, 'utf8'), root, seen);
  }
}

// bundledPackagesPlugin writes bundledPackages() for everything the build
// ships to <root>/.bundle/packages.json: the modules of every chunk, the
// source file of every asset, the same again for every web worker, and every
// file Vite inlined as a data: URL. The file sits beside dist, never in it,
// so it is not embedded in the stratum binary or published with the package;
// dash/.gitignore ignores it.
export function bundledPackagesPlugin() {
  let root;
  // A file Vite inlines as a data: URL ships inside a chunk or a stylesheet,
  // yet it is neither a module nor an emitted asset, so bundleIds never sees
  // it. That is any file under build.assetsInlineLimit (4 KiB by default)
  // that a stylesheet's url() or image-set(), a new URL(..., import.meta.url)
  // or index.html names. Vite asks assetsInlineLimit about each such file
  // before it decides, in the main build and in every worker build alike, so
  // config swaps the configured limit for this wrapper, which records the
  // file it is asked about, inlined or not, and answers as the configured
  // limit would. Two things inline a file without asking: a ?inline query on
  // the reference, which sourceCheck refuses, and a library build, which
  // checkBuild refuses. Like workerIds,
  // the set is never cleared: a watch rebuild reuses a file it already
  // inlined without asking again.
  const inlinedIds = new Set();
  let limit;
  const assetsInlineLimit = (file, content) => {
    inlinedIds.add(file);
    if (typeof limit === 'function') return limit(file, content);
    // Vite reads undefined as its own 4 KiB default, never as the configured
    // number, so a number is applied here. Vite's comparison also never
    // inlines a Git LFS pointer file; this one does not check.
    return limit === undefined ? undefined : content.length < Number(limit);
  };
  // Vite reads the options of the environment it builds, which a plugin can
  // still change after config runs: a later config hook, a configEnvironment
  // or configResolved hook, an environment's own build options. A library
  // build inlines every file without asking. Either way a file would ship
  // unseen, so the build fails when it starts instead.
  const checkBuild = ({ name, config: { build } }) => {
    if (build.assetsInlineLimit !== assetsInlineLimit) {
      const found = typeof build.assetsInlineLimit === 'function' ? 'another function' : String(build.assetsInlineLimit);
      throw new Error(`bundled packages: the ${name} environment's build.assetsInlineLimit is ${found}, not the `
        + 'wrapper that lists the files Vite inlines, so one would ship unseen by the license check; set '
        + 'build.assetsInlineLimit at the top level of vite.config.js, not in environments.<name>.build or a plugin hook');
    }
    if (build.lib) {
      throw new Error(`bundled packages: the ${name} environment's build.lib is set; `
        + 'a library build inlines every file unseen by the license check');
    }
  };
  // Reads each module's source before Vite's plugins rewrite it: any module
  // that writes a ?inline query fails the build, and checkImports runs on each
  // stylesheet before Vite's CSS plugin inlines its @imports.
  const sourceCheck = () => ({
    order: 'pre',
    filter: { code: { include: [HAS_IMPORT, INLINE_QUERY] } },
    handler(code, id) {
      const file = id.replace(/\?.*$/, '');
      refuseInline(file, code, root);
      if (STYLESHEET.test(id) && !NOT_COMPILED.test(id)) return checkImports(file, code, root, new Set([file]));
    },
  });
  // A worker is bundled by a build of its own, which runs worker.plugins
  // rather than plugins; the main build sees only the script it wrote, an
  // asset naming no source file. Each worker build records its ids here,
  // under its entry module. Vite runs it while the main build is still
  // loading and transforming modules, so every one is recorded before the
  // main generateBundle reads them. A worker only tree-shaken code started,
  // which Vite leaves out of dist, is still listed; so is one that a watch
  // rebuild (which reuses an unchanged worker without running its plugins)
  // stopped using. The check sees more than ships, never less.
  const workerIds = new Map();
  const workerPlugin = () => ({
    name: 'stratum:bundled-packages-worker',
    apply: 'build',
    buildStart() {
      checkBuild(this.environment);
    },
    transform: sourceCheck(),
    generateBundle(_options, bundle) {
      const entry = Object.values(bundle).find((output) => output.type === 'chunk' && output.isEntry);
      workerIds.set(entry.facadeModuleId, bundleIds(bundle, root));
    },
  });
  return {
    name: 'stratum:bundled-packages',
    apply: 'build',
    // Adds workerPlugin to the worker builds and swaps in the wrapped limit.
    // Vite's merge of the returned options keeps the worker plugins the
    // config already has, in either form, and keeps build.rollupOptions the
    // alias of rolldownOptions that a copy of build would break. In builder
    // mode the hook runs again for each environment and can find the
    // wrapper itself, which it never wraps again.
    config(config) {
      const configured = config.build?.assetsInlineLimit;
      if (configured !== assetsInlineLimit) limit = configured;
      return { worker: { plugins: () => [workerPlugin()] }, build: { assetsInlineLimit } };
    },
    configResolved(config) {
      root = config.root;
    },
    buildStart() {
      checkBuild(this.environment);
    },
    transform: sourceCheck(),
    async generateBundle(_options, bundle) {
      const ids = [...bundleIds(bundle, root), ...[...workerIds.values()].flat(), ...inlinedIds];
      const file = path.join(root, '.bundle', 'packages.json');
      await fs.mkdir(path.dirname(file), { recursive: true });
      await fs.writeFile(file, `${JSON.stringify(await bundledPackages(ids), null, 2)}\n`);
    },
  };
}
