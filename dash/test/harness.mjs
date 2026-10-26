// Shared test harness: executes the real modules and JSX render bodies with
// inert hooks and stubbed leaf widgets. Effects (network, terminal and DOM
// work) are intentionally out of scope — these tests concern what the render
// tree offers, not what it does on mount.
//
// harness({ stubs, real }) replaces or adds a module stub, matched by exact
// specifier or by suffix; the caller's stubs are consulted before the defaults
// below, so `{ 'react-router-dom': {...} }` overrides the default router.
// `signedIn` decides whether the stubbed /api/v1/me answers with IDENTITY or
// 401 — there is no role to pass any more.
// harness().module(relative) returns a module's live namespace — the same
// instance the components under test imported, which is how the registry test
// registers into the registry the shell actually reads.
import fs from 'node:fs/promises';
import vm from 'node:vm';
import { transformWithOxc } from 'vite';

export async function harness({ config = null, signedIn = false, view = 'list', search = '', stubs = {}, real = {} } = {}) {
  const jsx = (type, props, ...children) => ({ type, props: { ...props, children } });
  // Every fetch the modules under test make, so an assertion can read the
  // headers a write carried rather than only its result.
  const calls = [];
  const context = vm.createContext({
    console, URLSearchParams, AbortController, setTimeout, clearTimeout,
    window: {
      location: { href: '/login' },
      // AppShell's useMediaQuery: desktop, and never changing.
      matchMedia: () => ({ matches: true, addEventListener: () => {}, removeEventListener: () => {} }),
      addEventListener: () => {}, removeEventListener: () => {},
    },
    localStorage: { getItem: () => view },
    __jsx: jsx, __fragment: 'fragment',
    fetch: async (path, init) => (calls.push({ path, init: init || {} }), {
      ok: path === '/api/v1/config' ? !!config : signedIn,
      status: path === '/api/v1/config' ? (config ? 200 : 502) : (signedIn ? 200 : 401),
      json: async () => config,
      text: async () => JSON.stringify(path === '/api/v1/login'
        ? { user: IDENTITY, csrfToken: 'csrf' }
        : { ...IDENTITY, csrfToken: 'csrf' }),
    }),
  });
  const hooks = {
    useState: (initial) => [typeof initial === 'function' ? initial() : initial, () => {}],
    useEffect: () => {}, useRef: () => ({}), useCallback: (fn) => fn,
    useMemo: (fn) => fn(), useSyncExternalStore: (_subscribe, snapshot) => snapshot(),
  };
  function stub(values) {
    return new vm.SyntheticModule(Object.keys(values), function () {
      for (const [name, value] of Object.entries(values)) this.setExport(name, value);
    }, { context });
  }
  // The controls are stubbed by name so the assertions can find a
  // <Button title="Exec"> in the render tree without rendering one.
  const widgets = Object.fromEntries(['Button', 'Card', 'Table', 'TableHeader', 'TableBody',
    'TableRow', 'TableHead', 'TableCell', 'TextInput', 'Select', 'Badge', 'StatusBadge',
    'Tabs', 'TabButton', 'Progress', 'Sparkline', 'SplitterGroup', 'SplitterPane', 'Dialog',
  ].map((name) => [name, name]));
  const cache = new Map();
  async function load(relative) {
    if (cache.has(relative)) return cache.get(relative);
    let code = await fs.readFile(new URL('../src/' + relative, import.meta.url), 'utf8');
    if (relative.endsWith('.jsx')) {
      ({ code } = await transformWithOxc(code, relative, { jsx: { runtime: 'classic', pragma: '__jsx', pragmaFrag: '__fragment' } }));
    }
    const mod = new vm.SourceTextModule(code, { context });
    cache.set(relative, mod);
    await mod.link(async (specifier) => {
      for (const [match, values] of Object.entries(stubs)) {
        if (specifier === match || specifier.endsWith(match)) return stub(values);
      }
      // `real` maps a specifier suffix to a path under src/ to EXECUTE instead
      // of stubbing, for a module whose module-load side effects are the thing
      // under test (resources.jsx reserving its route ids).
      for (const [match, target] of Object.entries(real)) {
        if (specifier === match || specifier.endsWith(match)) return load(target);
      }
      if (specifier === 'react') return stub(hooks);
      if (specifier === 'react-router-dom') return stub({ useLocation: () => ({ pathname: '/login', search }), useNavigate: () => () => {} });
      if (specifier.endsWith('/controls.jsx')) return stub(widgets);
      if (specifier.endsWith('/session.js')) return load('session.js');
      if (specifier.endsWith('/data.js')) return load('data.js');
      if (specifier.endsWith('/cluster-context.js')) return load('cluster-context.js');
      if (specifier.endsWith('/registry.js')) return load('registry.js');
      if (specifier.endsWith('/version.js')) return load('version.js');
      if (specifier.endsWith('/k8s-rules.js')) return load('k8s-rules.js');
      if (specifier.endsWith('/crd-nav.js')) return load('crd-nav.js');
      if (specifier.endsWith('/icons.jsx')) return stub({ Icon: 'Icon' });
      if (specifier.endsWith('/terminal.jsx')) return stub({ ExecTerminal: 'ExecTerminal', NodeShell: 'NodeShell' });
      // Real, like the modules above: node-shell.js is DOM-free apart from one
      // guarded pagehide registration, and app.jsx imports its constants.
      if (specifier.endsWith('/node-shell.js')) return load('node-shell.js');
      // Every façade ui/shell.jsx exports that a screen imports. Keep this
      // complete rather than letting each test carry its own copy: a screen
      // gaining an import (nodeTint) otherwise breaks whichever
      // tests happen to stub this module themselves.
      if (specifier.endsWith('/shell.jsx')) {
        return stub({ Modal: 'Modal', StatusPill: 'StatusPill', PaneShell: 'PaneShell', Sparkline: 'Sparkline', Meter: 'Meter', nodeTint: () => '' });
      }
      if (specifier.endsWith('/resources.jsx')) return stub({ RESOURCES: {} });
      if (specifier.endsWith('/codeview.jsx')) return stub({ CodeView: 'CodeView' });
      if (specifier.endsWith('.svg')) return stub({ default: specifier });
      if (specifier.endsWith('/k8sutil.js')) return stub({ k8sGet: () => {}, k8sPost: () => {}, k8sPatch: () => {}, k8sDelete: () => {}, k8sFetch: async () => ({ ok: false, status: 0 }), listItems: () => [], age: () => '' });
      throw new Error('Unmocked dependency: ' + specifier);
    });
    await mod.evaluate();
    return mod;
  }
  const cfg = (await load('cluster-context.js')).namespace;
  const session = (await load('session.js')).namespace;
  return {
    session, calls, browser: context.window, resolveConfig: () => cfg.getConfig(),
    signIn: () => session.fetchMe(),
    module: async (relative) => (await load(relative)).namespace,
    component: async (file, name) => (await load(file)).namespace[name],
  };
}

// The identity /login and /me both report. It carries no role: the apiserver
// authorizes each request under the user's own token, so there is nothing
// role-shaped for the SPA to gate on.
const IDENTITY = { username: 'system:serviceaccount:demo:viewer', provider: 'kubernetes', groups: ['system:authenticated'] };

// Every node in a render tree the predicate accepts, props included — the
// trees here are inert, so a stubbed child's props are the only place its
// label, title or descriptor can be read.
export function collect(tree, pred) {
  if (!tree || typeof tree !== 'object') return [];
  if (Array.isArray(tree)) return tree.flatMap((item) => collect(item, pred));
  return [...(pred(tree) ? [tree] : []), ...Object.values(tree.props || {}).flatMap((value) => collect(value, pred))];
}
export function nodes(tree, type) { return collect(tree, (node) => node.type === type); }
export function text(tree) {
  if (typeof tree === 'string') return tree;
  // A count interpolated into a heading is a number child, and dropping it
  // silently turns "1 version drift" into " version drift" mid-assertion.
  if (typeof tree === 'number') return String(tree);
  if (!tree || typeof tree !== 'object') return '';
  if (Array.isArray(tree)) return tree.map(text).join('');
  return text(tree.props?.children);
}
