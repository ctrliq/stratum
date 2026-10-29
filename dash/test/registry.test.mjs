// The consumer test for the extension seam, shaped like a downstream consumer.
//
// It imports the package entry the way a downstream build will —
//   import { registerScreen, registerNav } from '@ctrliq/stratum-dash';
// which resolves to src/index.js through package.json's `exports` — registers
// a dummy screen and nav item, and then renders the REAL AppShell and the REAL
// route chain in app.jsx to prove the registration reaches both. Asserting
// against navGroups()/getScreen() alone would only prove the registry stores
// what it was handed; it would not notice the shell or app.jsx stopping to ask.
import assert from 'node:assert/strict';
import test from 'node:test';
import { harness, collect, nodes, text } from './harness.mjs';

// NOTE: arrays built inside the harness's vm realm carry that realm's Array
// prototype, so they are spread into host arrays before a deepEqual.

// What a consumer's registration module looks like. `render` returns an
// element-shaped object; the harness's trees are inert, so the assertions read
// the node the shell put in place rather than rendered markup.
const CLUSTERS = { type: 'ClustersScreen', props: { children: [] } };

// React Router's matching, for the two patterns app.jsx uses: segment by
// segment, `:name` binding one segment.
function matchPath(pattern, path) {
  const want = pattern.split('/'), got = path.split('/');
  if (want.length !== got.length) return null;
  const params = {};
  for (let i = 0; i < want.length; i++) {
    if (want[i].startsWith(':')) params[want[i].slice(1)] = got[i];
    else if (want[i] !== got[i]) return null;
  }
  return { params };
}

// screens/tabbed.jsx's tab lists, pinned against the real module below.
export const STORAGE_TABS = ['persistentvolumeclaims', 'persistentvolumes', 'storageclasses'];
export const RBAC_TABS = ['roles', 'rolebindings', 'clusterroles', 'clusterrolebindings', 'serviceaccounts'];

async function consumer({ pathname = '/clusters', search = '', group = 'Extras', stored } = {}) {
  const h = await harness({
    // The harness's localStorage answers every key with `view`, which is what
    // the shell reads its stored collapse state from.
    ...(stored ? { view: JSON.stringify(stored) } : {}),
    stubs: {
      // app.jsx routes off matchPath + useLocation; the screens themselves are
      // leaves here. Only the '/:route' pattern needs to match: parseLocation()
      // tries the CRD pattern first and must miss.
      'react-router-dom': {
        useLocation: () => ({ pathname, search }),
        useNavigate: () => () => {},
        matchPath,
        matchRoutes: () => null, Navigate: 'Navigate', Routes: 'Routes', Route: 'Route',
        BrowserRouter: 'BrowserRouter',
      },
      '/ui/shell.jsx': { AppShell: 'AppShell', Placeholder: 'Placeholder', Modal: 'Modal', StatusPill: 'StatusPill' },
      '/screens/overview.jsx': { Overview: 'Overview' },
      '/screens/infra.jsx': { Nodes: 'Nodes', Events: 'Events', Helm: 'Helm' },
      '/screens/workloads.jsx': { Workloads: 'Workloads', KIND_PLURAL: {} },
      '/screens/tabbed.jsx': { TabbedResources: 'TabbedResources', STORAGE_TABS, RBAC_TABS },
      '/screens/login.jsx': { Login: 'Login' },
      '/screens/terminal.jsx': { TerminalModal: 'TerminalModal' },
      '/ui/palette.jsx': { CommandPalette: 'CommandPalette' },
      '/ui/resource-list.jsx': { ResourceList: 'ResourceList' },
      '/ui/exec-logs-drawer.jsx': { ExecLogsDrawer: 'ExecLogsDrawer' },
      '/k8sutil.js': { k8sGet: async () => null, k8sPost: async () => ({ ok: false, status: 0 }),
        k8sPatch: async () => ({ ok: true }), k8sFetch: async () => ({ ok: false, status: 0 }),
        age: () => '', meta: (o) => (o && o.metadata) || {}, nameOf: () => '', nsOf: () => '', labelPairs: () => [] },
      './data.js': { K8S: {}, resetK8S: () => {} },
      './live.js': { loadLiveK8S: async () => {} },
    },
    // The REAL descriptor table: its route ids are what the seam has to
    // refuse, and a stub with an empty RESOURCES would prove nothing.
    real: { '/resources.jsx': 'resources.jsx' },
  });
  // The package entry, as a consumer imports it.
  const dash = await h.module('index.js');
  dash.registerScreen('clusters', { label: 'Clusters', render: (ctx) => ({ ...CLUSTERS, props: { ...CLUSTERS.props, ctx } }) });
  dash.registerNav({ group, id: 'clusters', label: 'Clusters', icon: 'server' });
  return { h, dash };
}

test('a registered nav item reaches the real sidebar, in its own group above Links', async () => {
  const { h, dash } = await consumer();
  const AppShell = await h.component('ui/shell.jsx', 'AppShell');

  const groups = [...dash.navGroups().map((g) => g.label)];
  assert.deepEqual(groups, ['', 'Workloads', 'Config', 'Network', 'Storage', 'Helm', 'Access Control',
    'Custom Resources', 'Extras', 'Links']);

  const tree = AppShell({ active: 'clusters', onNavigate: () => {}, crumbs: ['Clusters'], namespace: 'all-namespaces',
    config: { links: {} }, cluster: { name: 'kube' }, children: null });
  const row = collect(tree, (n) => n.props && n.props.label === 'Clusters');
  assert.equal(row.length, 1, 'the sidebar renders exactly one row for the registration');
  assert.equal(row[0].props.selected, true, 'and it is the selected row while its route is active');
  // The group header is rendered too, so the section is reachable when collapsed.
  assert.equal(collect(tree, (n) => n.props && n.props.className === 'grow nav-group-label'
    && text(n) === 'Extras').length, 1);
});

test('a registered screen reaches the real route chain, with the documented ctx', async () => {
  const { h } = await consumer();
  const Shell = await h.component('app.jsx', 'Shell');
  const tree = Shell({ config: { links: {} }, me: null, onSignOut: () => {} });

  const rendered = nodes(tree, 'ClustersScreen');
  assert.equal(rendered.length, 1, 'the registered screen is what /clusters renders');
  assert.equal(nodes(tree, 'Placeholder').length, 0, 'and it is not the "not built yet" placeholder');

  const ctx = rendered[0].props.ctx;
  assert.deepEqual([...Object.keys(ctx).sort()],
    ['cluster', 'config', 'data', 'me', 'namespace', 'onAction', 'onFlash', 'onNavigate', 'onRefresh', 'preselect', 'tab']);
  for (const fn of ['onNavigate', 'onAction', 'onFlash', 'onRefresh']) assert.equal(typeof ctx[fn], 'function', fn);
  assert.equal(ctx.namespace, 'all-namespaces');

  // The shell knows which nav row to light up, and the breadcrumb uses the
  // registered label rather than falling back to "Overview".
  const shell = nodes(tree, 'AppShell')[0];
  assert.equal(shell.props.active, 'clusters');
  assert.deepEqual([...shell.props.crumbs], ['Clusters']);
});

test('a registration cannot take one of the dashboard\'s own route ids', async () => {
  const { h, dash } = await consumer();
  const render = () => ({ type: 'Hijacked', props: {} });
  // A bespoke screen, which app.jsx's ordering already protects.
  assert.throws(() => dash.registerScreen('overview', { render }), /cannot be replaced/);
  // /login, which Boot routes before Shell ever consults the registry — so a
  // registration on it could never render, whatever the ordering says.
  assert.throws(() => dash.registerScreen('login', { render }), /cannot be replaced/);
  // The tabbed Storage and RBAC screens left the sidebar but still answer old
  // links, so they stay the dashboard's own.
  for (const id of ['storage', 'rbac']) {
    assert.throws(() => dash.registerScreen(id, { render }), /cannot be replaced/, id);
  }
  // And EVERY key of the real descriptor table. These reach the generic
  // ResourceList, which app.jsx resolves AFTER the registry, so this guard is
  // the only thing between a consumer and silently replacing one. Driven off
  // RESOURCES itself so a new descriptor cannot quietly escape the rule.
  const { RESOURCES } = await h.module('resources.jsx');
  const ids = Object.keys(RESOURCES);
  assert.ok(ids.length > 30, `expected the full descriptor table, got ${ids.length}`);
  assert.ok(ids.includes('deployments') && ids.includes('persistentvolumes'), 'non-sidebar ids present');
  for (const id of ids) {
    assert.throws(() => dash.registerScreen(id, { render }), /cannot be replaced/, id);
    assert.throws(() => dash.registerNav({ id, label: 'X' }), /already has a sidebar row/, id);
  }
});

test('a route id must be a usable path segment, or the router cannot reach it', async () => {
  const { dash } = await consumer();
  const render = () => null;
  //                                                    ↓ one segment, but the browser
  //                                                      normalises it away before the
  //                                                      router sees it
  for (const id of ['extra/clusters', '/clusters', 'a?b', 'a#b', 'two words', '.', '..']) {
    assert.throws(() => dash.registerScreen(id, { render }), /usable path segment/, id);
    assert.throws(() => dash.registerNav({ id, label: 'X' }), /usable path segment/, id);
  }
  // Not a string: `screens` is a Map, so the key 1 is never the '1' the router
  // hands back from the pathname — it would register and never resolve.
  for (const id of [1, true, {}, ['clusters']]) {
    assert.throws(() => dash.registerScreen(id, { render }), /must be a string/, String(id));
    assert.throws(() => dash.registerNav({ id, label: 'X' }), /must be a string/, String(id));
    assert.throws(() => dash.registerNav({ id, label: 'X', link: 'docs' }), /must be a string/, String(id));
  }
  // `:` is legal inside a segment — NAV's own link rows use it — and a link row
  // opens a URL rather than resolving to a route at all.
  dash.registerNav({ id: 'link:vendor-docs', label: 'Vendor docs', link: 'vendorDocs' });
  assert.ok(dash.navGroups().some((g) => g.items.some((it) => it.id === 'link:vendor-docs')));
});

test('reserving a route id a registration already took fails, whichever loads first', async () => {
  // The documented consumer order imports its registrations BEFORE the app, so
  // resources.jsx reserves its ids AFTER registerScreen has run. The guard
  // cannot assume call order.
  const { h, dash } = await consumer();
  dash.registerScreen('deployments-but-registered-first', { render: () => null });
  const registry = await h.module('registry.js');
  assert.throws(() => registry.reserveRoutes(['deployments-but-registered-first']),
    /a registration already took it/);
  // And it is not a one-shot: reserving ids nothing claimed still works.
  registry.reserveRoutes(['something-new']);
  assert.throws(() => dash.registerScreen('something-new', { render: () => null }), /cannot be replaced/);
});

test('the seam rejects a registration it cannot render, reach, or tell apart', async () => {
  const { dash } = await consumer();
  assert.throws(() => dash.registerScreen('broken', {}), /render\(ctx\) function/);
  assert.throws(() => dash.registerScreen('', { render: () => null }), /an id and a render/);
  assert.throws(() => dash.registerNav({ id: 'nolabel' }), /an id and a label/);
  // `clusters` is registered by consumer() above — a second registration of it
  // would otherwise win silently, which is how two consumers disagree in prod.
  assert.throws(() => dash.registerScreen('clusters', { render: () => null }), /already registered/);
  assert.throws(() => dash.registerNav({ id: 'clusters', label: 'Clusters' }), /already has a sidebar row/);
});

test('a percent-encoded id is rejected: the router decodes it, so the shell would look up another id', async () => {
  const { dash } = await consumer();
  const render = () => null;
  // React Router hands back 'a/b' for '/a%2Fb', so getScreen() would miss.
  for (const id of ['a%2Fb', 'a%2fb', '%', '100%']) {
    assert.throws(() => dash.registerScreen(id, { render }), /usable path segment/, id);
    assert.throws(() => dash.registerNav({ id, label: 'X' }), /usable path segment/, id);
  }
});

test('a link row and a screen cannot share an id, in either registration order', async () => {
  const render = () => null;
  // Screen first, then the link row.
  {
    const { dash } = await consumer();
    dash.registerScreen('docs-vendor', { render });
    assert.throws(() => dash.registerNav({ id: 'docs-vendor', label: 'Docs', link: 'vendorDocs' }),
      /'docs-vendor'.*registered screen/);
  }
  // Link row first, then the screen.
  {
    const { dash } = await consumer();
    dash.registerNav({ id: 'docs-vendor', label: 'Docs', link: 'vendorDocs' });
    assert.throws(() => dash.registerScreen('docs-vendor', { render }), /'docs-vendor'.*link row/);
  }
  // The documented screen-plus-nav-row pairing still works, in both orders.
  {
    const { dash } = await consumer();
    dash.registerNav({ id: 'a', label: 'A' });
    dash.registerScreen('a', { render });
    dash.registerScreen('b', { render });
    dash.registerNav({ id: 'b', label: 'B' });
  }
});

test('a backslash is rejected: URL parsing reads it as "/", so the segment is not one', async () => {
  const { dash } = await consumer();
  const render = () => null;
  for (const id of ['a\\b', '\\', 'a\\']) {
    assert.throws(() => dash.registerScreen(id, { render }), /usable path segment/, id);
    assert.throws(() => dash.registerNav({ id, label: 'X' }), /usable path segment/, id);
  }
});

test('a registration with no group joins the top group', async () => {
  const { dash } = await consumer({ group: '' });
  const groups = [...dash.navGroups()];
  assert.deepEqual([...groups[0].items.map((it) => it.id)], ['overview', 'nodes', 'namespaces', 'events', 'clusters']);
  assert.ok(!groups.slice(1).some((g) => g.items.some((it) => it.id === 'clusters')));
});

// The sidebar mirrors Lens's cluster menu, in Lens's order.
const SIDEBAR = [
  ['', ['overview', 'nodes', 'namespaces', 'events']],
  ['Workloads', ['workloads', 'pods', 'deployments', 'daemonsets', 'statefulsets', 'replicasets',
    'replicationcontrollers', 'jobs', 'cronjobs']],
  ['Config', ['configmaps', 'secrets', 'resourcequotas', 'limitranges', 'horizontalpodautoscalers',
    'poddisruptionbudgets', 'priorityclasses', 'runtimeclasses', 'leases',
    'mutatingwebhookconfigurations', 'validatingwebhookconfigurations']],
  ['Network', ['services', 'endpoints', 'endpointslices', 'ingress', 'ingressclasses', 'networkpolicies']],
  ['Storage', ['persistentvolumeclaims', 'persistentvolumes', 'storageclasses']],
  ['Helm', ['helm', 'helmcharts', 'helmchartconfigs']],
  ['Access Control', ['serviceaccounts', 'clusterroles', 'roles', 'clusterrolebindings', 'rolebindings']],
  ['Custom Resources', ['crds']],
  ['Links', ['link:docs', 'link:grafana', 'link:pro']],
];

test('the sidebar lists every page in Lens order', async () => {
  const { h } = await consumer();
  const registry = await h.module('registry.js');
  const got = [...registry.navGroups()].filter((g) => g.label !== 'Extras')
    .map((g) => [g.label, [...g.items.map((it) => it.id)]]);
  assert.deepEqual(got, SIDEBAR);
  const ids = [...registry.navGroups()].map((g) => g.id).filter(Boolean);
  assert.equal(new Set(ids).size, ids.length, 'group ids key collapse state, so they must be unique');
});

test('every sidebar row renders a real screen, never the placeholder', async () => {
  for (const id of SIDEBAR.flatMap(([, ids]) => ids).filter((id) => !id.startsWith('link:'))) {
    const { h } = await consumer({ pathname: `/${id}` });
    const Shell = await h.component('app.jsx', 'Shell');
    const tree = Shell({ config: { links: {} }, me: null, onSignOut: () => {} });
    assert.equal(nodes(tree, 'Placeholder').length, 0, `/${id} renders a screen`);
    assert.equal(nodes(tree, 'AppShell')[0].props.active, id, `/${id} lights its own row`);
  }
});

test('each sidebar row shows a real glyph, the same one the palette shows for its kind', async () => {
  const { h } = await consumer();
  const { GLYPHS } = await h.module('icons-data.js');
  const { RESOURCES } = await h.module('resources.jsx');
  const registry = await h.module('registry.js');
  for (const g of registry.navGroups()) {
    for (const it of g.items) {
      assert.ok(GLYPHS[it.icon], `${it.id}: no glyph named ${it.icon}`);
      if (RESOURCES[it.id]) assert.equal(it.icon, RESOURCES[it.id].icon, it.id);
    }
  }
});

test('an old Storage or RBAC link lights the row of the tab it shows', async () => {
  const cases = [
    ['/storage', '?tab=persistentvolumes', 'persistentvolumes'],
    ['/storage', '', 'persistentvolumeclaims'],
    ['/storage', '?tab=nonsense', 'persistentvolumeclaims'],
    ['/rbac', '?tab=clusterroles', 'clusterroles'],
    ['/rbac', '', 'roles'],
  ];
  for (const [pathname, search, row] of cases) {
    const { h } = await consumer({ pathname, search });
    const Shell = await h.component('app.jsx', 'Shell');
    const tree = Shell({ config: { links: {} }, me: null, onSignOut: () => {} });
    assert.equal(nodes(tree, 'TabbedResources').length, 1, `${pathname}${search} still renders the tab screen`);
    assert.equal(nodes(tree, 'AppShell')[0].props.active, row, `${pathname}${search}`);
  }
});

test('a CRD instance list lights its Custom Resources row', async () => {
  const { h } = await consumer({ pathname: '/crds/a.example.com/v1/widgets' });
  const Shell = await h.component('app.jsx', 'Shell');
  const shell = nodes(Shell({ config: { links: {} }, me: null, onSignOut: () => {} }), 'AppShell')[0];
  assert.equal(shell.props.active, 'crds/a.example.com/v1/widgets');
  assert.deepEqual([...shell.props.crdGroups], [], 'no CRD rows until the list has loaded');
});

function renderShell(h, props) {
  return h.component('ui/shell.jsx', 'AppShell').then((AppShell) => AppShell({
    onNavigate: () => {}, crumbs: ['x'], namespace: 'all-namespaces',
    config: { links: {} }, cluster: { name: 'kube' }, children: null, ...props }));
}
const CRD_GROUPS = [
  { group: 'a.example.com', items: [{ id: 'crds/a.example.com/v1/gadgets', label: 'Gadget' }, { id: 'crds/a.example.com/v1/widgets', label: 'Widget' }] },
  { group: 'b.example.com', items: [{ id: 'crds/b.example.com/v2/things', label: 'Thing' }] },
];

test('Custom Resources lists a sub-group per CRD API group, closed until it holds the page', async () => {
  const { h } = await consumer();
  const subsOf = (tree) => collect(tree, (n) => typeof n.type === 'function' && n.type.name === 'CrdSubgroup');
  const rowsOf = (sub) => collect(sub.type(sub.props), (n) => typeof n.type === 'function' && n.type.name === 'NavItem');

  let subs = subsOf(await renderShell(h, { active: 'overview', crdGroups: CRD_GROUPS }));
  assert.deepEqual([...subs.map((n) => n.props.sub.group)], ['a.example.com', 'b.example.com']);
  assert.deepEqual([...subs.map((n) => rowsOf(n).length)], [0, 0], 'closed by default');

  subs = subsOf(await renderShell(h, { active: 'crds/a.example.com/v1/widgets', crdGroups: CRD_GROUPS }));
  const rows = rowsOf(subs[0]);
  assert.deepEqual([...rows.map((r) => r.props.label)], ['Gadget', 'Widget'], 'the group holding the page opens');
  assert.deepEqual([...rows.map((r) => r.props.selected)], [false, true]);
  assert.equal(rowsOf(subs[1]).length, 0, 'and only that group');

  // A stored choice wins over the default, either way.
  const closed = subs[0].type({ ...subs[0].props, stored: false });
  assert.equal(collect(closed, (n) => typeof n.type === 'function' && n.type.name === 'NavItem').length, 0);
  const open = subs[1].type({ ...subs[1].props, stored: true });
  assert.equal(collect(open, (n) => typeof n.type === 'function' && n.type.name === 'NavItem').length, 1);

  // A row navigates to the CRD's instance route by its id.
  const went = [];
  const navTree = await renderShell(h, { active: 'crds/a.example.com/v1/widgets', crdGroups: CRD_GROUPS, onNavigate: (id) => went.push(id) });
  rowsOf(subsOf(navTree)[0])[1].props.onClick();
  assert.deepEqual(went, ['crds/a.example.com/v1/widgets']);
});

test('every group renders under a distinct key', async () => {
  const { h } = await consumer();
  const tree = await renderShell(h, { active: 'overview', crdGroups: [] });
  const keys = collect(tree, (n) => typeof n.props?.className === 'string' && /^nav-group( |$)/.test(n.props.className))
    .map((n) => n.props.key);
  const registry = await h.module('registry.js');
  const shown = [...registry.navGroups()].filter((g) => g.items.some((it) => !it.link)).length;
  assert.equal(keys.length, shown, 'one rendered group per group with a row to show');
  assert.equal(new Set(keys).size, keys.length);
});

test('the tab lists the Storage and RBAC links map from are the real ones', async () => {
  const { h } = await consumer();
  const tabbed = await h.module('screens/tabbed.jsx');
  assert.deepEqual([...tabbed.STORAGE_TABS], STORAGE_TABS);
  assert.deepEqual([...tabbed.RBAC_TABS], RBAC_TABS);
});

test('the two new kinds list their columns from the object', async () => {
  const { h } = await consumer();
  const { RESOURCES } = await h.module('resources.jsx');
  const cells = (id, o) => Object.fromEntries(RESOURCES[id].columns.map((c) => [c.key, text(c.render(o))]));

  const slice = cells('endpointslices', {
    metadata: { name: 'web-x1', namespace: 'shop', labels: { 'kubernetes.io/service-name': 'web' } },
    addressType: 'IPv4', endpoints: [{}, {}, {}], ports: [{ port: 80, protocol: 'TCP' }, { protocol: 'UDP' }],
  });
  assert.equal(slice.service, 'web');
  assert.equal(slice.atype, 'IPv4');
  assert.equal(slice.eps, '3');
  assert.equal(slice.ports, '80/TCP, */UDP');

  const rc = cells('replicationcontrollers', { metadata: { name: 'old', namespace: 'legacy' }, spec: { replicas: 3 }, status: { readyReplicas: 2 } });
  assert.equal(rc.desired, '3');
  assert.equal(rc.ready, '2');
  assert.equal(cells('replicationcontrollers', { metadata: { name: 'd' }, spec: {} }).desired, '1', 'replicas defaults to 1');

  assert.ok(RESOURCES.replicationcontrollers.ops.includes('scale'), 'an RC scales through its scale subresource');
  assert.equal(RESOURCES.endpointslices.path, '/apis/discovery.k8s.io/v1/endpointslices');
  assert.equal(RESOURCES.replicationcontrollers.path, '/api/v1/replicationcontrollers');
});

test('a CRD served at several versions lights its row from any version in the URL', async () => {
  const { h } = await consumer();
  const subsOf = (tree) => collect(tree, (n) => typeof n.type === 'function' && n.type.name === 'CrdSubgroup');
  const rowsOf = (sub) => collect(sub.type(sub.props), (n) => typeof n.type === 'function' && n.type.name === 'NavItem');
  const subs = subsOf(await renderShell(h, { active: 'crds/a.example.com/v2/widgets', crdGroups: CRD_GROUPS }));
  const rows = rowsOf(subs[0]);
  assert.deepEqual([...rows.map((r) => [r.props.label, r.props.selected])], [['Gadget', false], ['Widget', true]],
    'the v1 row owns the v2 page too');
  assert.equal(rowsOf(subs[1]).length, 0, 'and no other group opens');
  const went = [];
  const again = subsOf(await renderShell(h, { active: 'crds/a.example.com/v2/widgets', crdGroups: CRD_GROUPS, onNavigate: (id) => went.push(id) }));
  rowsOf(again[0])[1].props.onClick();
  assert.deepEqual(went, ['crds/a.example.com/v1/widgets'], 'clicking still goes to the version the row names');
});

test('a registered group sharing a built-in group id as its label keeps its own collapse state', async () => {
  const { h, dash } = await consumer({ group: 'storage', stored: { 'id:storage': true } });
  const tree = await renderShell(h, { active: 'overview', crdGroups: [] });
  const groups = collect(tree, (n) => typeof n.props?.className === 'string' && /^nav-group( |$)/.test(n.props.className));
  const byLabel = (label) => groups.find((n) => text(collect(n, (c) => c.props?.className === 'grow nav-group-label')) === label);
  assert.match(byLabel('Storage').props.className, /collapsed/, 'the built-in Storage group reads its stored state');
  assert.doesNotMatch(byLabel('storage').props.className, /collapsed/, 'the registered "storage" group does not share it');
  assert.notEqual(byLabel('Storage').props.key, byLabel('storage').props.key);
  assert.ok([...dash.navGroups()].some((g) => g.label === 'storage' && !g.id), 'the registration is a group of its own');
});

test('a section collapsed under its bare label by an earlier build stays collapsed', async () => {
  const { h } = await consumer({ stored: { Workloads: true } });
  const tree = await renderShell(h, { active: 'overview', crdGroups: [] });
  const workloads = collect(tree, (n) => typeof n.props?.className === 'string' && /^nav-group( |$)/.test(n.props.className))
    .find((n) => text(collect(n, (c) => c.props?.className === 'grow nav-group-label')) === 'Workloads');
  assert.match(workloads.props.className, /collapsed/);
});

test('Get Pro shows only when the server serves its link, and opens it in a new tab', async () => {
  const { h } = await consumer();
  const proRow = (tree) => collect(tree, (n) => n.props && n.props.label === 'Get Pro');

  assert.equal(proRow(await renderShell(h, { active: 'overview' })).length, 0,
    'a server that serves no pro link, such as the PRO hub, gets no row');

  const url = 'https://ciq.com/products/stratum';
  const rows = proRow(await renderShell(h, { active: 'overview', config: { links: { pro: url } } }));
  assert.equal(rows.length, 1);
  const opened = [];
  h.browser.open = (...args) => { opened.push(args); };
  rows[0].props.onClick();
  assert.deepEqual(opened.map((a) => [...a]), [[url, '_blank', 'noopener,noreferrer']]);
});
