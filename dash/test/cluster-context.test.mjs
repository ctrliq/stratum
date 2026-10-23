import assert from 'node:assert/strict';
import test from 'node:test';
import { harness, text } from './harness.mjs';

// cluster-context.js resolves /api/v1/config through the global fetch, once per
// getConfig({ force: true }), so the stub can be re-pointed between sub-cases.
const served = { body: null, status: 200, throw: null };
globalThis.fetch = async () => {
  if (served.throw) { const e = served.throw; served.throw = null; throw e; }
  return { ok: served.body != null && served.status === 200, status: served.status, json: async () => served.body };
};
console.warn = () => {};

const { getConfig, currentConfig, apiBase } = await import('../src/cluster-context.js');

const DEFAULT = {
  mode: 'standalone', version: '', k8sBase: '/k8s',
  auth: { required: false, providers: [] }, features: {}, links: {},
};

// A config nobody served — a network error, a 404, the dev proxy's 502 — used
// to fall back silently to the login-free default. That pin was deliberately
// changed: in production the failure must be visible, because an
// operator looking at a dashboard that cannot see its cluster is looking at
// stale data, not at a working one. The login-free fallback survives only in
// explicit development mode, where a bare `kubectl proxy` with nothing on 8443
// is the documented setup. Tests (and embedders) stand in for vite's
// import.meta.env.DEV with the __STRATUM_DEV__ global, which cluster-context
// reads beside it.
test('a production config failure is visible, and only dev mode keeps the login-free default', async () => {
  for (const [label, set, why] of [
    ['a network error', () => { served.throw = new Error('ECONNREFUSED'); }, 'ECONNREFUSED'],
    ['a 404', () => { served.body = null; served.status = 404; }, '404'],
    ['a 502', () => { served.body = null; served.status = 502; }, '502'],
  ]) {
    set();
    const c = await getConfig({ force: true });
    assert.ok(c, `${label} must still resolve a config the SPA can boot on`);
    assert.equal(typeof c.configError, 'string', `${label} must surface a visible error state`);
    assert.ok(c.configError.length > 0, `${label}'s error state must carry a message`);
    assert.ok(c.configError.includes(why), `${label}'s error must name what failed, not just "an error"`);
    assert.notDeepEqual(c, DEFAULT, `${label} must not resolve to the silent login-free default`);
    assert.equal((currentConfig() || {}).configError, c.configError, 'the synchronous config view must expose the same error state');
    assert.equal(apiBase(), '/k8s', `${label} must leave the documented /k8s prefix for requests`);
  }

  globalThis.__STRATUM_DEV__ = true;
  try {
    for (const [label, set] of [
      ['a network error', () => { served.throw = new Error('ECONNREFUSED'); }],
      ['a 404', () => { served.body = null; served.status = 404; }],
      ['a 502', () => { served.body = null; served.status = 502; }],
    ]) {
      set();
      const c = await getConfig({ force: true });
      assert.deepEqual(c, DEFAULT, `${label} in development mode must keep the login-free default`);
    }
  } finally { delete globalThis.__STRATUM_DEV__; }
  served.status = 200;
});

// The visible half of the production contract: a config nobody served must
// reach the operator, not render a working-looking login-free dashboard. Boot
// itself cannot run under the harness's inert effects, so app.jsx exports the
// pure BootError view Boot renders for a configError, and the pin reads it
// against the REAL cluster-context realm the harness serves a 502 config from.
// The screens are stubbed by name (registry.test.mjs does the same); what
// matters here is Boot's error path, not any screen.
test('Boot renders the config failure, and a config nobody served offers no writes', async () => {
  const h = await harness({
    stubs: {
      'react-router-dom': {
        useLocation: () => ({ pathname: '/', search: '' }),
        useNavigate: () => () => {},
        matchPath: () => null, matchRoutes: () => null,
        BrowserRouter: 'BrowserRouter', Navigate: 'Navigate', Routes: 'Routes', Route: 'Route',
      },
      '/ui/shell.jsx': { AppShell: 'AppShell', Placeholder: 'Placeholder', Modal: 'Modal', StatusPill: 'StatusPill' },
      '/screens/overview.jsx': { Overview: 'Overview' },
      '/screens/infra.jsx': { Nodes: 'Nodes', Events: 'Events', Helm: 'Helm' },
      '/screens/workloads.jsx': { Workloads: 'Workloads', KIND_PLURAL: {} },
      '/screens/tabbed.jsx': { TabbedResources: 'TabbedResources', STORAGE_TABS: ['pvcs'], RBAC_TABS: ['roles'] },
      '/screens/login.jsx': { Login: 'Login' },
      '/screens/terminal.jsx': { TerminalModal: 'TerminalModal' },
      '/ui/palette.jsx': { CommandPalette: 'CommandPalette' },
      '/ui/resource-list.jsx': { ResourceList: 'ResourceList' },
      '/ui/exec-logs-drawer.jsx': { ExecLogsDrawer: 'ExecLogsDrawer' },
      './resources.jsx': { RESOURCES: {}, crdDescriptor: () => null },
      './data.js': { K8S: {}, resetK8S: () => {} },
      './live.js': { loadLiveK8S: async () => {} },
    },
  });
  const cfg = await h.resolveConfig();     // the harness's config stub is null → the proxy's 502
  assert.ok(cfg.configError.includes('502'), 'the failure the harness served must ride the resolved config');
  assert.equal(h.session.canWrite(), false, 'a config the SPA could not fetch offers no writes');

  const app = await h.module('app.jsx');
  const cc = await h.module('cluster-context.js');
  const rendered = text(app.BootError({ config: cc.currentConfig() }));
  assert.ok(rendered.includes('502'), 'the boot error must name what failed, not just "an error"');
  assert.ok(rendered.includes('could not read its configuration'), 'the boot error must say what is wrong');
  assert.equal(rendered.toLowerCase().includes('sign in'), false, 'the boot error must not offer a login the unreachable server cannot honor');
});

// What a SERVED config means is unchanged by the failure-handling change: a
// standalone config merges over the default and keeps what it says about auth,
// base and features; a mode this build does not implement counts as unserved.
// (The silent default this last row resolves to is pinned above as dev-only
// behavior; when unsupported modes gain their own visible error, this row
// changes with that decision, not silently.)
test('a served standalone config keeps its own auth, base and features', async () => {
  served.status = 200;
  served.body = {
    mode: 'standalone', k8sBase: '/clusters/a/k8s', version: '1.2.3',
    auth: { required: true, providers: ['token'] }, features: { nodeShell: true },
  };
  const c = await getConfig({ force: true });
  assert.deepEqual(c, {
    mode: 'standalone', version: '1.2.3', k8sBase: '/clusters/a/k8s',
    auth: { required: true, providers: ['token'] }, features: { nodeShell: true }, links: {},
  }, 'a served standalone config keeps its own auth, base and features');

  for (const body of [{ mode: 'managed', k8sBase: '/managed/k8s' }, { mode: 'standalone', auth: 'invalid' },
    { mode: 'standalone' }, []]) {
    served.body = body;
    const invalid = await getConfig({ force: true });
    assert.ok(invalid.configError, 'an unsupported or malformed production config must be visible');
    assert.notDeepEqual(invalid, DEFAULT, 'invalid production config must not enable the login-free default');
  }
});
