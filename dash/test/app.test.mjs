// The loader wiring inside Shell: the part no render-tree assertion can see,
// because the harness keeps effects inert and the first load runs inside an
// effect. This file runs Shell's effects with a recording loader stub.
//
// The defect it pins: app.jsx imports the loader as a module namespace
// (`import * as liveLoader from './live.js'`), and Shell holds an auto-refresh
// toggle state (`const [live, setLive]`). A same-named import was once
// shadowed by that state for the entire body of Shell, so every
// `live.loadLiveK8S()` call site resolved to the boolean and the dashboard
// loaded no data at all — silently, with the whole suite green, because no
// test ran an effect. This test runs the effects, so a name collision between
// an import and hook state fails here instead of emptying every screen.
import assert from 'node:assert/strict';
import test from 'node:test';
import { harness } from './harness.mjs';

// The hooks Shell needs, with effects that RUN (once, no cleanup) instead of
// the harness's inert default. Everything else stays the harness's own realm.
const runningHooks = {
  useState: (initial) => [typeof initial === 'function' ? initial() : initial, () => {}],
  useEffect: (fn) => fn(),
  useRef: () => ({}),
  useCallback: (fn) => fn,
  useMemo: (fn) => fn(),
  useSyncExternalStore: (_subscribe, snapshot) => snapshot(),
};

test("Shell's first load reaches the live loader (no import is shadowed by hook state)", async () => {
  let loads = 0;
  const h = await harness({
    stubs: {
      react: runningHooks,
      'react-router-dom': {
        BrowserRouter: 'BrowserRouter', useNavigate: () => () => {},
        useLocation: () => ({ pathname: '/login', search: '' }),
        matchPath: () => null, Navigate: 'Navigate', Routes: 'Routes', Route: 'Route',
      },
      '/ui/shell.jsx': { AppShell: 'AppShell', Placeholder: 'Placeholder' },
      '/screens/overview.jsx': { Overview: 'Overview' },
      '/screens/infra.jsx': { Nodes: 'Nodes', Events: 'Events', Helm: 'Helm' },
      '/screens/workloads.jsx': { Workloads: 'Workloads', KIND_PLURAL: {} },
      '/screens/tabbed.jsx': { TabbedResources: 'TabbedResources', STORAGE_TABS: [], RBAC_TABS: [] },
      '/screens/login.jsx': { Login: 'Login' },
      '/screens/terminal.jsx': { TerminalModal: 'TerminalModal' },
      '/ui/palette.jsx': { CommandPalette: 'CommandPalette' },
      '/ui/resource-list.jsx': { ResourceList: 'ResourceList' },
      '/ui/exec-logs-drawer.jsx': { ExecLogsDrawer: 'ExecLogsDrawer' },
      './resources.jsx': { RESOURCES: {}, crdDescriptor: () => null },
      // The loader the test records. refreshVerdict is named because
      // applyOutcome resolves it only when a load settles.
      './live.js': {
        loadLiveK8S: async () => { loads += 1; return { answered: true, unauthorized: false, stale: [] }; },
        refreshVerdict: () => ({ stamp: false, message: null }),
      },
    },
    // resetK8S() and the K8S shell run for real: the first-load effect reads
    // both before reaching the loader.
    real: { './data.js': 'data.js' },
  });

  const Shell = await h.component('app.jsx', 'Shell');
  Shell({ config: { mode: 'standalone', auth: { required: false } }, me: null, onSignOut: async () => {} });
  await new Promise((resolve) => setTimeout(resolve, 0));

  assert.equal(loads, 1,
    'Shell ran its first load through the live.js module — zero here means a name collision is shadowing the import');
});
