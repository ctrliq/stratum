// App — router + boot + the shell. Routes:
//   /login                            sign-in (when the server reports auth.required)
//   /:route                           a screen (route ids = screen ids / RESOURCES keys)
//   /crds/:group/:version/:plural     a CRD's instances (dynamic descriptor)
//   /                                 → /overview
// Query params: ?ns=<namespace> (filter), ?node=<name> (open node detail),
// ?name=<ns>/<name> (preselect a ResourceList row), ?tab=<id> (Storage/RBAC tab).
//
// One cluster, one apiserver: the dashboard is served by the cluster it shows.
// Live state is loaded once on mount and re-polled every 15 s.
import { useState, useEffect, useCallback, useMemo, useRef } from 'react';
import { BrowserRouter, useNavigate, useLocation, matchPath, Navigate, Routes, Route } from 'react-router-dom';
import { Button } from './ui/controls.jsx';
import { AppShell, Placeholder } from './ui/shell.jsx';
import { Overview } from './screens/overview.jsx';
import { Nodes, Events, Helm } from './screens/infra.jsx';
import { Workloads, KIND_PLURAL } from './screens/workloads.jsx';
import { TabbedResources, STORAGE_TABS, RBAC_TABS } from './screens/tabbed.jsx';
import { Login } from './screens/login.jsx';
import { TerminalModal } from './screens/terminal.jsx';
import { CommandPalette } from './ui/palette.jsx';
import { ResourceList } from './ui/resource-list.jsx';
import { ExecLogsDrawer } from './ui/exec-logs-drawer.jsx';
import { RESOURCES, crdDescriptor } from './resources.jsx';
import { crdRoute, crdNavGroups } from './crd-nav.js';
// k8sutil as a namespace for the same reason live.js is one: a consumer stub
// may declare only part of the transport (registry.test.mjs's stub has no
// k8sFetch), and a named import of a missing export fails the module link.
// The names read at module load are the ones every realm declares; the
// sign-out flow reads k8sFetch only when it actually runs.
import * as k8sutil from './k8sutil.js';
const { k8sPatch, k8sGet } = k8sutil;
import { K8S, resetK8S } from './data.js';
// The loader's namespace, not a named import of refreshVerdict beside
// loadLiveK8S: a consumer may stub ./live.js with loadLiveK8S alone
// (registry.test.mjs), and a named import of an export the stub does not
// declare fails the module link. The namespace resolves the extra name only
// when a load actually settles. The name is liveLoader, not live: Shell holds
// an auto-refresh toggle state called `live`, and a same-named import would be
// shadowed by it for every loader call inside Shell — a silent
// never-loads-any-data defect (dash/test/app.test.mjs pins the collision).
import * as liveLoader from './live.js';
import * as nodeShell from './node-shell.js';
import { getConfig } from './cluster-context.js';
import { fetchMe, logout, setUnauthorizedHandler, clearSession, currentSessionRevision, currentAuthRevision, isSessionCurrent, subscribeRemoteSessionChanges } from './session.js';
import { getScreen, screenLabel } from './registry.js';

// ---- sign-out: the shells go before the cookie does -------------------------
// The logout POST clears the session cookie server-side, and every Kubernetes
// call the SPA makes rides that cookie through the proxy — a DELETE sent after
// it is just a 401, and an open host-shell Job would sit out its full
// activeDeadlineSeconds on the node. So the flow first attempts each open
// shell's Job delete — best-effort acceleration of the teardown the TTL
// controller owns (a finished Job is deleted by its spec.ttlSecondsAfterFinished,
// node-shell.js) — bounded per shell so a hung proxy cannot hold sign-out
// hostage, reporting per-shell failures instead of swallowing them; logout
// runs once the attempts settle (the bound aborts a hung request, so logout
// waits no longer than the bound). The panels' own stop functions still run at
// unmount afterwards and 401 harmlessly.
const SHELL_DELETE_BOUND_MS = 3000; // per shell; sign-out never waits longer than this for one DELETE

// One bounded delete attempt against the shell's Job. k8sFetch never throws,
// but the bound rides the caller's signal either way: the abort is what ends a
// hung request at the deadline, and the failure it produces is reported, not
// swallowed — a Job nobody could delete is named here, never silently dropped.
async function deleteShellJob({ pod, path, node, retire }, deadlineMs) {
  const abort = new AbortController();
  const timer = setTimeout(() => abort.abort(), deadlineMs);
  const who = node ? `node ${node} (${pod})` : pod;
  try {
    const r = await k8sutil.k8sFetch('DELETE', `${path}/${pod}${nodeShell.NODE_SHELL_DELETE_QUERY}`, { signal: abort.signal, quiet401: true });
    if (r.ok || r.status === 404) retire?.();
    if (!r.ok && r.status !== 404) {
      console.error(`Stratum: host shell delete failed for ${who} — ${r.error || 'HTTP ' + r.status}`);
    }
  } catch (e) {
    console.error(`Stratum: host shell delete failed for ${who} — ${(e && e.message) || e}`);
  } finally {
    clearTimeout(timer);
  }
}

// The sign-out flow onSignOut runs: every open host shell gets a bounded
// delete against its Job while the session still stands, then logout clears
// the cookie once. A changed session returns superseded so Boot also leaves
// its new UI alone.
export async function signOut({ deadlineMs = SHELL_DELETE_BOUND_MS } = {}) {
  const revision = currentAuthRevision();
  const pending = nodeShell.pendingNodeShellCreates?.() || [];
  if (pending.length) {
    let timer;
    await Promise.race([
      Promise.allSettled(pending),
      new Promise((resolve) => { timer = setTimeout(resolve, deadlineMs); }),
    ]);
    clearTimeout(timer);
    if (revision !== currentAuthRevision()) return { superseded: true };
    if ((nodeShell.pendingNodeShellCreates?.() || []).length) {
      console.error('Stratum: a host shell create did not finish before logout; its Job may need cleanup');
    }
  }
  const shells = nodeShell.openNodeShells?.() || [];
  await Promise.all(shells.map((shell) => deleteShellJob(shell, deadlineMs)));
  // Cleanup failures stay local to explicit sign-out. A genuinely replaced
  // session must still keep its shell ownership and cookie.
  if (revision !== currentAuthRevision()) return { superseded: true };
  nodeShell.endNodeShellSession();
  const result = await logout();
  return { superseded: !!result.stale };
}

// Bespoke screens (custom value); every other route falls through to the generic
// ResourceList driven by the registry (src/resources.jsx).
const BESPOKE_TITLES = {
  overview: 'Overview', helm: 'Helm Releases', nodes: 'Nodes', events: 'Events',
  workloads: 'Workloads', storage: 'Storage', rbac: 'RBAC', ingress: 'Ingresses',
};
const titleFor = (route) => BESPOKE_TITLES[route] || screenLabel(route)
  || (RESOURCES[route] && RESOURCES[route].label) || 'Overview';

// The sidebar row a location lights. The Storage and RBAC tab screens left the
// sidebar for one row per kind, so an old /storage or /rbac link lights the
// row of the tab it shows; a CRD's instance list lights its Custom Resources row.
export function activeNavId(loc, tab) {
  if (loc.kind === 'crd') return `crds/${loc.crd.group}/${loc.crd.version}/${loc.crd.plural}`;
  if (loc.route === 'storage') return STORAGE_TABS.includes(tab) ? tab : STORAGE_TABS[0];
  if (loc.route === 'rbac') return RBAC_TABS.includes(tab) ? tab : RBAC_TABS[0];
  return loc.route;
}

function parseLocation(pathname) {
  let m = matchPath('/crds/:group/:version/:plural', pathname);
  if (m) return { kind: 'crd', route: 'crds', crd: m.params };
  m = matchPath('/:route', pathname);
  if (m) return { kind: 'screen', route: m.params.route };
  return null;
}

export function App() {
  return (
    <BrowserRouter>
      <Boot />
    </BrowserRouter>
  );
}

// Boot: config → (auth) session. Renders /login alone when a session is
// required and missing; otherwise the shell.
function Boot() {
  const [config, setConfig] = useState(null);
  const [me, setMe] = useState(null);
  const [phase, setPhase] = useState('loading'); // loading | login | ready | error
  const navigate = useNavigate();
  const location = useLocation();

  useEffect(() => {
    let alive = true;
    (async () => {
      const cfg = await getConfig();
      if (!alive) return;
      setConfig(cfg);
      // A config nobody served is a visible failure in production: stop in the
      // error phase instead of rendering a dashboard that cannot see its
      // cluster. Dev mode resolves the plain login-free default (no
      // configError) and boots exactly as before.
      if (cfg.configError) { setPhase('error'); return; }
      if (cfg.auth && cfg.auth.required) {
        const revision = currentSessionRevision();
        const who = await fetchMe();
        if (!alive) return;
        if (!isSessionCurrent(revision)) { setPhase('login'); return; }
        if (!who) { setPhase('login'); return; }
        setMe(who);
      }
      if (!alive) return;
      setPhase('ready');
    })();
    return () => { alive = false; };
  }, []);

  // Any 401 from the server API → /login, remembering where we were.
  useEffect(() => {
    setUnauthorizedHandler(() => {
      if (!(config && config.auth && config.auth.required)) return;
      clearSession();
      nodeShell.endNodeShellSession();
      setMe(null); setPhase('login');
      navigate('/login', { replace: true, state: { from: location.pathname + location.search } });
    });
    const unsubscribe = subscribeRemoteSessionChanges(() => {
      if (!config?.auth?.required) return;
      setMe(null); setPhase('login');
      navigate('/login', { replace: true, state: { from: location.pathname + location.search } });
    });
    return () => { setUnauthorizedHandler(null); unsubscribe(); };
  }, [config, navigate, location.pathname, location.search]);

  const onSignedIn = useCallback(async (user, _csrf, revision = currentSessionRevision()) => {
    const who = user || await fetchMe();
    if (!who || !isSessionCurrent(revision)) return;
    setMe(who);
    setPhase('ready');
  }, []);
  const onSignOut = useCallback(async () => {
    // Shells first (bounded, reported), then the logout that clears the
    // cookie — after it the shell deletes would only ever be 401s.
    const outcome = await signOut();
    if (outcome.superseded) return;
    setMe(null); setPhase('login');
    navigate('/login', { replace: true });
  }, [navigate]);

  if (phase === 'loading' || !config) return <BootSplash text="Connecting to the cluster…" />;
  // A resolved configError renders as the error, never as a working-looking
  // dashboard: an operator whose /api/v1/config cannot be fetched is looking
  // at an SPA that cannot see its cluster, and must see that.
  if (phase === 'error' || config.configError) return <BootError config={config} />;
  if (phase === 'login') {
    if (location.pathname !== '/login') return <Navigate to="/login" replace state={{ from: location.pathname + location.search }} />;
    return <Login config={config} onSignedIn={onSignedIn} />;
  }
  return (
    <Routes>
      <Route path="/login" element={config.auth && config.auth.required && !me
        ? <Login config={config} onSignedIn={onSignedIn} />
        : <Navigate to="/" replace />} />
      <Route path="*" element={<Shell config={config} me={me} onSignOut={onSignOut} />} />
    </Routes>
  );
}

function BootSplash({ text }) {
  return (
    <div className="login-wrap">
      <div className="dim" style={{ fontSize: 13 }}>{text}</div>
    </div>
  );
}

// The visible boot failure: /api/v1/config could not be fetched, so the SPA
// cannot know what its cluster is, and the shell would show stale data as if
// it were live. Exported pure so the config tests can pin it without running
// Boot's effects.
export function BootError({ config }) {
  return (
    <div className="login-wrap">
      <div className="dim" style={{ fontSize: 13 }}>
        The dashboard could not read its configuration, so it cannot show the cluster. {config && config.configError}
      </div>
    </div>
  );
}

// The shell: one component for every route so the drawer, toast and live state
// survive navigation. Route params are derived from the pathname.
export function Shell({ config, me, onSignOut }) {
  const data = K8S;
  const navigate = useNavigate();
  const location = useLocation();
  const loc = useMemo(() => parseLocation(location.pathname), [location.pathname]);
  const params = useMemo(() => new URLSearchParams(location.search), [location.search]);
  const namespace = params.get('ns') || 'all-namespaces';
  const nodeSel = params.get('node');
  const preselect = params.get('name');
  const tab = params.get('tab');

  const [paletteOpen, setPaletteOpen] = useState(false);
  const [term, setTerm] = useState(null);
  const [toast, setToast] = useState(null);
  const [drawerTabs, setDrawerTabs] = useState([]); // pod exec/logs bottom drawer
  const [drawerActive, setDrawerActive] = useState(null);
  const [live, setLive] = useState(true);
  const [updated, setUpdated] = useState(null);   // null until a load answers
  const [staleSlices, setStaleSlices] = useState([]);   // K8S slice keys showing last-good data
  const [loadingData, setLoadingData] = useState(true);
  const [dyn, setDyn] = useState(null); // dynamic CRD-instance descriptor for the /crds/:g/:v/:p route
  const [crdNav, setCrdNav] = useState([]); // the sidebar's Custom Resources sub-groups

  const cluster = data.CLUSTER;

  // One load's outcome, applied the same way whoever ran the load (first load,
  // poll tick, Refresh, a write's reload): the stale slice names drive the
  // toggle's label, and only a load that answered moves the refreshed-at time.
  // The decision itself is refreshVerdict's, in live.js.
  const applyOutcome = useCallback((outcome) => {
    setStaleSlices((outcome && outcome.stale) || []);
    const verdict = liveLoader.refreshVerdict(outcome);
    if (verdict.stamp) setUpdated(Date.now());
    return verdict;
  }, []);

  // First load.
  const loadedRef = useRef(false);
  useEffect(() => {
    if (loadedRef.current) return;
    loadedRef.current = true;
    let alive = true;
    (async () => {
      if (!window.__STRATUM_LIVE) resetK8S();
      try { applyOutcome(await liveLoader.loadLiveK8S()); } catch (e) { console.error('Stratum: live data load failed —', e); }
      if (!alive) return;
      window.__STRATUM_K8S_VERSION = (data.CLUSTER_INFO && data.CLUSTER_INFO.version) || '';
      setLoadingData(false);
    })();
    return () => { alive = false; };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  // Live auto-refresh: 15s AFTER the previous load settles, never every 15s.
  // One load fans out to a request per node, so on a slow cluster a fixed
  // interval overlaps polls and lets a late reply clobber newer state.
  // (live.js serialises concurrent callers and bounds each request; this only
  // decides the cadence.) `loadingData` gates it so the first poll is not
  // queued up behind an initial load that has not finished.
  useEffect(() => {
    if (!live || loadingData) return;
    let alive = true, id = null;
    const tick = async () => {
      try { applyOutcome(await liveLoader.loadLiveK8S()); } catch { /* keep last good data */ }
      if (alive) id = setTimeout(tick, 15000);
    };
    id = setTimeout(tick, 15000);
    return () => { alive = false; clearTimeout(id); };
  }, [live, loadingData, applyOutcome]);

  // The CRD list carries every definition's full schema and changes rarely, so
  // it refreshes on its own minute-long cadence rather than with every live
  // poll. A failed read keeps the rows it already has.
  const loadCrdNav = useCallback(async () => {
    const list = await k8sGet('/apis/apiextensions.k8s.io/v1/customresourcedefinitions', null);
    if (list && Array.isArray(list.items)) setCrdNav(crdNavGroups(list.items));
  }, []);
  useEffect(() => {
    if (loadingData) return undefined;
    loadCrdNav();
    if (!live) return undefined;
    const id = setInterval(loadCrdNav, 60000);
    return () => clearInterval(id);
  }, [live, loadingData, loadCrdNav]);

  // ⌘K command palette
  useEffect(() => {
    const k = (e) => { if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); setPaletteOpen((o) => !o); } };
    window.addEventListener('keydown', k);
    return () => window.removeEventListener('keydown', k);
  }, []);

  // Dynamic CRD route: fetch the definition, build a descriptor for its instances.
  useEffect(() => {
    if (!loc || loc.kind !== 'crd') { setDyn(null); return; }
    let alive = true;
    const { group, version, plural } = loc.crd;
    setDyn(null);
    (async () => {
      const crd = await k8sGet(`/apis/apiextensions.k8s.io/v1/customresourcedefinitions/${plural}.${group}`, null);
      if (!alive) return;
      if (crd && crd.spec) {
        const d = crdDescriptor(crd);
        // pin to the version in the URL
        d.path = `/apis/${group}/${version}/${plural}`;
        setDyn(d);
      } else {
        // The CRD itself is unreadable (RBAC, or it does not exist): still list the
        // path the URL names. `kind` is only used for prose, so singularise the plural.
        const kind = plural.replace(/ies$/, 'y').replace(/s$/, '');
        setDyn({ label: `${plural} (${group})`, kind: kind.charAt(0).toUpperCase() + kind.slice(1), path: `/apis/${group}/${version}/${plural}`, namespaced: false, crd: true,
          columns: RESOURCES.crds.columns.slice(0, 1), ops: ['edit', 'delete'], unresolved: true });
      }
    })();
    return () => { alive = false; };
  }, [loc]);

  const flash = useCallback((msg) => { setToast(msg); setTimeout(() => setToast(null), 2800); }, []);

  // ---- navigation (everything goes through the router) ----
  const hrefFor = useCallback((id, extra = {}) => {
    const q = new URLSearchParams();
    if (namespace !== 'all-namespaces') q.set('ns', namespace);
    for (const [k, v] of Object.entries(extra)) { if (v == null || v === '') q.delete(k); else q.set(k, v); }
    const qs = q.toString();
    return `/${id}${qs ? `?${qs}` : ''}`;
  }, [namespace]);
  const go = useCallback((id, extra) => navigate(hrefFor(id, extra)), [navigate, hrefFor]);
  const openNode = useCallback((name) => go('nodes', { node: name }), [go]);
  const setNamespace = useCallback((ns) => {
    const q = new URLSearchParams(location.search);
    if (ns && ns !== 'all-namespaces') q.set('ns', ns); else q.delete('ns');
    const qs = q.toString();
    navigate(`${location.pathname}${qs ? `?${qs}` : ''}`, { replace: true });
  }, [navigate, location.pathname, location.search]);
  const refresh = useCallback(async () => {
    try {
      loadCrdNav();
      const verdict = applyOutcome(await liveLoader.loadLiveK8S());
      if (verdict.message) flash(verdict.message);   // an ended session reports through sign-in, not a toast
    } catch { flash('Refresh failed — keeping last data'); }
  }, [flash, applyOutcome, loadCrdNav]);

  // open a pod's logs/exec in the bottom drawer (obj = raw K8s pod object)
  const openPod = useCallback((obj, sub) => {
    const ns = obj?.metadata?.namespace, name = obj?.metadata?.name;
    if (!ns || !name) return;
    const key = `${ns}/${name}`;
    const containers = ((obj.spec && obj.spec.containers) || []).map((c) => c.name);
    setDrawerTabs((prev) => prev.some((t) => t.key === key)
      ? prev.map((t) => (t.key === key ? { ...t, sub } : t))
      : [...prev, { key, ns, pod: name, containers, sub }]);
    setDrawerActive(key);
  }, []);
  const closeTab = useCallback((key) => setDrawerTabs((prev) => {
    const next = prev.filter((t) => t.key !== key);
    setDrawerActive((a) => (a === key ? (next[next.length - 1] || {}).key || null : a));
    return next;
  }), []);
  // open a node's host shell in the SAME bottom drawer as pods. Node tabs are
  // kind:'node' and always sub:'exec' — there is no log stream for a host shell,
  // so the drawer hides the Logs|Exec toggle for them. The pane key must stay
  // stable: NodeShell creates a privileged debug pod on mount and deletes it on
  // unmount, so a changing key would churn a pod on every tab switch.
  const openNodeShell = useCallback((name) => {
    if (!name) return;
    const key = `node/${name}`;
    setDrawerTabs((prev) => (prev.some((t) => t.key === key)
      ? prev
      : [...prev, { key, kind: 'node', node: name, sub: 'exec' }]));
    setDrawerActive(key);
  }, []);
  const setSub = useCallback((key, sub) => setDrawerTabs((prev) => prev.map((t) => (t.key === key ? { ...t, sub } : t))), []);

  // All real actions. Writes on individual resources (edit/scale/restart/delete/
  // create) live in the generic ResourceList detail pane; this handles the
  // contextual actions the bespoke screens + palette raise.
  const onAction = useCallback(async (kind, obj) => {
    if (kind === 'refresh') return refresh();
    if (kind === 'logs') return openPod(obj, 'logs');
    if (kind === 'exec') { if (obj && obj.metadata) return openPod(obj, 'exec'); return setTerm(obj && obj.pod ? { type: 'pod', ns: obj.ns, pod: obj.pod, container: obj.container } : { type: 'pod-pick', filter: obj && obj.name }); }
    if (kind === 'node-shell') return openNodeShell(obj && obj.name);
    if (kind === 'open-crd') {
      const spec = (obj && obj.spec) || {};
      return navigate(`/${crdRoute(spec)}`);
    }
    // real node cordon/uncordon (Nodes screen) — PATCH spec.unschedulable
    if (kind === 'cordon' || kind === 'uncordon') {
      const node = obj && obj.name;
      if (!node) return;
      const r = await k8sPatch(`/api/v1/nodes/${node}`, { spec: { unschedulable: kind === 'cordon' } }, 'merge');
      flash(r.ok ? `${node} ${kind === 'cordon' ? 'cordoned — no new pods will schedule' : 'uncordoned — schedulable again'}` : `Error: ${r.error}`);
      if (r.ok) { try { applyOutcome(await liveLoader.loadLiveK8S()); } catch { /* keep last good */ } }
      return;
    }
  }, [flash, openPod, openNodeShell, navigate, refresh]);

  // ---- root redirect ----
  if (!loc) return <Navigate to="/overview" replace />;

  const route = loc.route;
  const counts = {
    events: (data.EVENTS || []).filter((e) => e.type === 'Warning').length,
    pods: (data.PODS || []).length || (data.NODES || []).reduce((a, n) => a + (n.pods || 0), 0),
    nodes: (data.NODES || []).length,
    workloads: (data.WORKLOADS || []).length,
  };
  const crumbs = loc.kind === 'crd' ? ['Custom Resources', dyn ? dyn.kind : loc.crd.plural] : [titleFor(route)];

  // A consumer's route (src/registry.js). Resolved below AFTER the bespoke
  // screens and BEFORE the generic RESOURCES fallthrough: a registration can
  // add a route, never shadow one of this dashboard's own.
  const registered = getScreen(route);

  let screen;
  const loading = loadingData && !window.__STRATUM_LIVE;
  if (loc.kind === 'crd') screen = (
    <div className="content-wide col" style={{ gap: 10, height: '100%', minHeight: 0 }}>
      <div className="row" style={{ gap: 8 }}>
        <Button variant="ghost" size="sm" onClick={() => go('crds')}>← Custom Resources</Button>
        {dyn && dyn.unresolved && <span className="dim" style={{ fontSize: 12 }}>definition not readable — listing {dyn.path}</span>}
      </div>
      {dyn ? <ResourceList descriptor={dyn} namespace={namespace} onAction={onAction} onFlash={flash} preselect={preselect} />
        : <div className="dim" style={{ fontSize: 13 }}>Resolving {loc.crd.plural}.{loc.crd.group}…</div>}
    </div>
  );
  else if (route === 'overview') screen = <Overview data={data} cluster={cluster} onNavigate={go} onOpenNode={openNode} onRefresh={refresh} />;
  else if (route === 'helm') screen = <Helm data={data} />;
  else if (route === 'nodes') screen = <Nodes data={data} onAction={onAction} initialNode={nodeSel} onSelectNode={(n) => go('nodes', { node: n })} />;
  else if (route === 'events') screen = <Events data={data} namespace={namespace} />;
  else if (route === 'workloads') screen = <Workloads data={data} namespace={namespace} onOpen={(w) => go(KIND_PLURAL[w.kind] || 'deployments', { name: `${w.ns}/${w.name}` })} />;
  else if (route === 'storage') screen = <TabbedResources title="Storage" tabs={STORAGE_TABS} tab={tab || STORAGE_TABS[0]} onTab={(t) => go('storage', { tab: t })} namespace={namespace} onAction={onAction} onFlash={flash} preselect={preselect} />;
  else if (route === 'rbac') screen = <TabbedResources title="RBAC" tabs={RBAC_TABS} tab={tab || RBAC_TABS[0]} onTab={(t) => go('rbac', { tab: t })} namespace={namespace} onAction={onAction} onFlash={flash} preselect={preselect} />;
  else if (route === 'ingress') screen = <ResourceList descriptor={RESOURCES.ingresses} namespace={namespace} onAction={onAction} onFlash={flash} preselect={preselect} />;
  else if (registered) screen = registered.render({ data, cluster, config, me, namespace, tab, preselect,
    onNavigate: go, onAction, onFlash: flash, onRefresh: refresh });
  else if (RESOURCES[route]) screen = <ResourceList descriptor={RESOURCES[route]} namespace={namespace} onAction={onAction} onFlash={flash} preselect={preselect} />;
  else screen = <Placeholder title={titleFor(route)} />;

  return (
    <>
      <AppShell active={activeNavId(loc, tab)} crdGroups={crdNav} onNavigate={go} counts={counts}
        crumbs={crumbs} namespace={namespace} onNamespace={setNamespace}
        namespaces={data.NAMESPACES || ['all-namespaces']}
        live={live} updated={updated} stale={staleSlices} onToggleLive={() => setLive((v) => !v)}
        onPalette={() => setPaletteOpen(true)}
        cluster={cluster} config={config} me={me} onSignOut={onSignOut}>
        <div style={{ display: 'flex', flexDirection: 'column', height: '100%', minHeight: 0 }}>
          <div style={{ flex: 1, minHeight: 0, overflow: 'auto', position: 'relative' }}>
            {loading && (
              <div className="dim" style={{ position: 'absolute', top: 8, right: 16, fontSize: 11.5, zIndex: 2 }}>Loading…</div>
            )}
            {screen}
          </div>
          <ExecLogsDrawer tabs={drawerTabs} activeKey={drawerActive} onActivate={setDrawerActive}
            onCloseTab={closeTab} onSetSub={setSub}
            onCloseDrawer={() => { setDrawerTabs([]); setDrawerActive(null); }} />
        </div>
      </AppShell>

      <CommandPalette open={paletteOpen} onClose={() => setPaletteOpen(false)}
        onNavigate={go} onAction={onAction} />
      <TerminalModal target={term} data={data} onClose={() => setTerm(null)} />

      {toast && <div className="toast" role="status">{toast}</div>}
    </>
  );
}
