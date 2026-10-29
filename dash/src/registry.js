// ============================================================
// registry.js — the extension seam.
//
// This tree is the dashboard. A downstream build consumes it as a versioned npm
// package (`@ctrliq/stratum-dash`) and adds its own screens through the two
// register* functions below, so no downstream-only source has to live in this
// repo for the shell to grow a route. See dash/README.md § "The extension seam"
// for why a build-time flag was rejected.
//
// Register and render. There is no plugin framework here: no lifecycle events,
// no dependency ordering, no unregister. A consumer imports the package,
// calls registerScreen()/registerNav() at module load (before its own
// createRoot()), and the shell picks them up on the first render.
// ============================================================

// The sidebar, and the single source of truth for it. ids that are neither a
// bespoke screen (app.jsx) nor a registration resolve to a generic ResourceList
// via the descriptor registry (src/resources.jsx).
//
// Grouped and ordered the way Lens orders its cluster menu. A group's `id` keys
// its collapse state, so a label can change without resetting what a user
// collapsed. Cluster-wide pages (Nodes, Namespaces, Events) sit at the top
// with Overview rather than in Lens's later positions. The Custom Resources group
// lists Definitions here; the shell appends one sub-group per CRD API group
// from the live CRD list (src/crd-nav.js).
//
// Not in the sidebar but still routes: `storage` and `rbac`, the older tabbed
// screens (each tab is a row below), and each CRD's instance list under /crds.
const NAV = [
  { id: 'top', label: '', items: [
    { id: 'overview', label: 'Overview', icon: 'grid' },
    { id: 'nodes', label: 'Nodes', icon: 'server', count: 'nodes' },
    { id: 'namespaces', label: 'Namespaces', icon: 'ns' },
    { id: 'events', label: 'Events', icon: 'bell', count: 'events' },
  ]},
  { id: 'workloads', label: 'Workloads', items: [
    { id: 'workloads', label: 'Overview', icon: 'layers', count: 'workloads' },
    { id: 'pods', label: 'Pods', icon: 'box', count: 'pods' },
    { id: 'deployments', label: 'Deployments', icon: 'cube' },
    { id: 'daemonsets', label: 'DaemonSets', icon: 'server' },
    { id: 'statefulsets', label: 'StatefulSets', icon: 'layers' },
    { id: 'replicasets', label: 'ReplicaSets', icon: 'copy' },
    { id: 'replicationcontrollers', label: 'Replication Controllers', icon: 'copy' },
    { id: 'jobs', label: 'Jobs', icon: 'log' },
    { id: 'cronjobs', label: 'CronJobs', icon: 'clock' },
  ]},
  { id: 'config', label: 'Config', items: [
    { id: 'configmaps', label: 'ConfigMaps', icon: 'sliders' },
    { id: 'secrets', label: 'Secrets', icon: 'key' },
    { id: 'resourcequotas', label: 'Resource Quotas', icon: 'gauge' },
    { id: 'limitranges', label: 'Limit Ranges', icon: 'sliders' },
    { id: 'horizontalpodautoscalers', label: 'Horizontal Pod Autoscalers', icon: 'arrowUp' },
    { id: 'poddisruptionbudgets', label: 'Pod Disruption Budgets', icon: 'shield' },
    { id: 'priorityclasses', label: 'Priority Classes', icon: 'layers' },
    { id: 'runtimeclasses', label: 'Runtime Classes', icon: 'cube' },
    { id: 'leases', label: 'Leases', icon: 'clock' },
    { id: 'mutatingwebhookconfigurations', label: 'Mutating Webhooks', icon: 'link' },
    { id: 'validatingwebhookconfigurations', label: 'Validating Webhooks', icon: 'shield' },
  ]},
  { id: 'network', label: 'Network', items: [
    { id: 'services', label: 'Services', icon: 'share' },
    { id: 'endpoints', label: 'Endpoints', icon: 'link' },
    { id: 'endpointslices', label: 'Endpoint Slices', icon: 'link' },
    { id: 'ingress', label: 'Ingresses', icon: 'globe' },
    { id: 'ingressclasses', label: 'Ingress Classes', icon: 'globe' },
    { id: 'networkpolicies', label: 'Network Policies', icon: 'shield' },
  ]},
  { id: 'storage', label: 'Storage', items: [
    { id: 'persistentvolumeclaims', label: 'Persistent Volume Claims', icon: 'database' },
    { id: 'persistentvolumes', label: 'Persistent Volumes', icon: 'database' },
    { id: 'storageclasses', label: 'Storage Classes', icon: 'layers' },
  ]},
  { id: 'helm', label: 'Helm', items: [
    { id: 'helm', label: 'Releases', icon: 'helm' },
    { id: 'helmcharts', label: 'Helm Charts', icon: 'helm' },
    { id: 'helmchartconfigs', label: 'Helm Chart Configs', icon: 'helm' },
  ]},
  { id: 'access', label: 'Access Control', items: [
    { id: 'serviceaccounts', label: 'Service Accounts', icon: 'user' },
    { id: 'clusterroles', label: 'Cluster Roles', icon: 'shield' },
    { id: 'roles', label: 'Roles', icon: 'shield' },
    { id: 'clusterrolebindings', label: 'Cluster Role Bindings', icon: 'link' },
    { id: 'rolebindings', label: 'Role Bindings', icon: 'link' },
  ]},
  { id: 'crds', label: 'Custom Resources', crds: true, items: [
    { id: 'crds', label: 'Definitions', icon: 'puzzle' },
  ]},
  { id: 'links', label: 'Links', items: [
    { id: 'link:docs', label: 'Documentation', icon: 'book', link: 'docs' },
    { id: 'link:grafana', label: 'Grafana', icon: 'activity', link: 'grafana' },
    { id: 'link:pro', label: 'Get Pro', icon: 'zap', link: 'pro' },
  ]},
];

// The route ids this dashboard owns, so a registration cannot take one.
//
// app.jsx resolves its bespoke screens BEFORE registrations, so /overview was
// never at risk — but the generic ResourceList resolves AFTER the registry, so
// every descriptor key in resources.jsx was. Refusing the id fails at the
// registration instead of quietly rendering the wrong screen, and a consumer
// that genuinely needs to replace a screen changes it here, in the open.
//
// What is known statically: NAV's ids, `crds` (which owns
// /crds/:group/:version/:plural as well as its own list), `login` (Boot
// routes it before Shell ever consults getScreen, so a registration on it
// could never render) and `storage`/`rbac` (bespoke tab screens that left the
// sidebar but still answer old links). The generic resource ids are NOT known here —
// resources.jsx owns that list and declares it through reserveRoutes() below,
// because this file cannot import it: resources.jsx → ui/shell.jsx → here.
const RESERVED = new Set([...NAV.flatMap((g) => g.items.map((it) => it.id)), 'crds', 'login', 'storage', 'rbac']);

// A route id must be ONE path segment that addresses something. app.jsx's
// parseLocation() matches '/:route' and hrefFor() drops the id straight into
// it, so an id the URL cannot carry verbatim is a registration that passes and
// then never renders — the shell root-redirects instead.
//   - anything but a string: `screens` is a Map, so the key 1 is never the
//     '1' the router hands back from the pathname
//   - a path or query delimiter: '/a/b' does not match '/:route'
//   - a backslash: URL parsing reads it as '/', so 'a\b' is two segments
//   - '%': React Router percent-decodes the param, so 'a%2Fb' comes back as
//     'a/b' and getScreen() misses. Rejected rather than encoded at both ends.
//   - '.' or '..': one segment, but the browser normalises '/..' to '/' before
//     the router ever sees it
// `:` stays legal inside a segment, which is what NAV's own `link:docs` rows
// rely on.
const badId = (id) => typeof id !== 'string' || /[/\\?#%\s]/.test(id) || id === '.' || id === '..';
const BAD_ID_MSG = 'is not a usable path segment — a route id must be a string, and cannot contain / \\ ? # % or whitespace, or be . or ..';

const screens = new Map();
const navItems = [];

const taken = (id) => screens.has(id) || navItems.some((n) => n.item.id === id);

// Declare ids as this dashboard's own. resources.jsx calls it with its
// descriptor keys at module load. A consumer's registration module can be
// imported BEFORE the app (that is the documented order), so this cannot
// assume it runs first: it rejects an id a registration already took rather
// than trusting call order.
export function reserveRoutes(ids) {
  for (const id of ids) {
    RESERVED.add(id);
    if (taken(id)) throw new Error(`reserveRoutes: '${id}' is one of the dashboard's own screens, but a registration already took it`);
  }
}

// Register a route. `id` is the path segment (`/clusters`) and the nav id that
// selects it; `render(ctx)` returns the screen's element and is called from the
// shell's render body, so it must be a pure function of `ctx` (below).
export function registerScreen(id, { label, render } = {}) {
  if (!id || typeof render !== 'function') throw new Error('registerScreen: an id and a render(ctx) function are required');
  if (badId(id)) throw new Error(`registerScreen: '${id}' ${BAD_ID_MSG}`);
  if (RESERVED.has(id)) throw new Error(`registerScreen: '${id}' is one of the dashboard's own screens and cannot be replaced`);
  if (screens.has(id)) throw new Error(`registerScreen: '${id}' is already registered`);
  // AppShell gives a row's `link` precedence over its route, so a screen on a
  // link row's id would have no way in.
  if (navItems.some((n) => n.item.link && n.item.id === id)) throw new Error(`registerScreen: '${id}' is already a link row, which would shadow the screen`);
  screens.set(id, { label: label || id, render });
}

export function getScreen(id) { return screens.get(id) || null; }
export function screenLabel(id) { const s = screens.get(id); return s ? s.label : null; }

// Add a sidebar row. `group` names the section by its label; an unknown label
// creates that section above "Links" (external links stay last). `id` should
// match a registerScreen() id — a row whose id is neither a registration nor a
// RESOURCES key navigates to a Placeholder.
export function registerNav({ group = '', ...item } = {}) {
  if (!item.id || !item.label) throw new Error('registerNav: an id and a label are required');
  // A link row opens a URL and never resolves to a route, so the segment rule
  // does not apply to it — but the id is still compared against the active
  // route as a string to light the row up, so it must be one either way.
  if (typeof item.id !== 'string') throw new Error(`registerNav: the id must be a string, got ${typeof item.id}`);
  if (!item.link && badId(item.id)) throw new Error(`registerNav: '${item.id}' ${BAD_ID_MSG}`);
  // Only a duplicate ROW is a conflict. The normal pairing is one
  // registerScreen plus one registerNav on the same id, so this check must not
  // look at the screen table, or it rejects every correct
  // registration. Only a LINK row may collide with a screen (below).
  if (RESERVED.has(item.id) || navItems.some((n) => n.item.id === item.id)) {
    throw new Error(`registerNav: '${item.id}' already has a sidebar row`);
  }
  if (item.link && screens.has(item.id)) throw new Error(`registerNav: '${item.id}' is a registered screen, and a link row would shadow it`);
  navItems.push({ group, item });
}

// The sidebar the shell renders: NAV above, with every registration merged in.
// Called on every render, so it must not mutate NAV.
export function navGroups() {
  const out = NAV.map((g) => ({ ...g, items: [...g.items] }));
  for (const { group, item } of navItems) {
    let g = out.find((x) => x.label === group);
    if (!g) {
      g = { label: group, items: [] };
      const links = out.findIndex((x) => x.label === 'Links');
      out.splice(links < 0 ? out.length : links, 0, g);
    }
    g.items.push(item);
  }
  return out;
}
