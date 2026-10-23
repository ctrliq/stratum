import assert from 'node:assert/strict';
import test from 'node:test';
import fs from 'node:fs/promises';
import vm from 'node:vm';
import { transformWithOxc } from 'vite';

// ============================================================
// signout.test.mjs — the sign-out flow deletes shells before the cookie
// clears.
//
// Signing out clears the session cookie server-side (POST /api/v1/logout),
// and every Kubernetes call the SPA makes rides that same session through
// the proxy. So the open host shells' DELETEs have to go out BEFORE the
// logout call — after it they are just 401s and the privileged pods are
// stranded until activeDeadlineSeconds ends them. The flow's contract:
//
//   1. every registered open host shell gets a DELETE before logout,
//   2. the attempt is bounded — a DELETE that never answers is abandoned
//      at the deadline and logout proceeds at the bound, not after the
//      transport's own 15s request deadline (and not never),
//   3. a failed DELETE is reported per shell, and never prevents sign-out,
//   4. logout still happens exactly once, and the flow ends signed out,
//   5. superseded cleanup and logout continuations leave a new session alone,
//   6. nothing unregistered is ever deleted — a foreign stratum-node-shell-*
//      Job (or pod) this session never created gets no DELETE from sign-out
//      or pagehide; ownership comes only from the SPA's own create POST.
//
// harness.mjs runs effects inert by design and the ordering above lives in
// the sign-out flow itself, so these tests run the real app.jsx in a vm of
// their own — one that has the platform pieces that code touches (the
// global timers the bound rides, AbortController) and a fetch stub of its
// own. The component delegates to one seam the tests drive directly:
// signOut, the flow onSignOut runs, with the bound injectable so a test
// never waits on a real deadline.
// ============================================================
const IMAGE = 'ghcr.io/ctrliq/stratum/busybox:1.37.0'; // a server-supplied payload image
const IDENTITY = { username: 'op', provider: 'kubernetes', groups: ['system:authenticated'] };

// Mock timers replace the test realm's setTimeout per test; the wall-clock
// failsafes below must survive that, so the real pair is captured at load.
const realSetTimeout = globalThis.setTimeout;
const realClearTimeout = globalThis.clearTimeout;
const settle = async () => { for (let i = 0; i < 20; i++) await Promise.resolve(); };
// A hung flow must fail the test, not hang the run — and the failsafe must
// not outlive the race, or the file lingers on a real timer after passing.
const bounded = (promise, ms) => new Promise((resolve, reject) => {
  const timer = realSetTimeout(() => reject(new Error(`the sign-out flow did not settle within ${ms}ms`)), ms);
  Promise.resolve(promise).then(
    (v) => { realClearTimeout(timer); resolve(v); },
    (e) => { realClearTimeout(timer); reject(e); },
  );
});

// One vm context + module graph per test, so the shell registry each test
// populates stays its own.
async function openDash({ cookies = { value: 'initial-session', csrf: 'csrf' }, locks, storage } = {}) {
  const calls = [];    // every fetch, in order: { method, url, init }
  const reports = [];  // console.error/warn records: [channel, text]
  const created = {};  // node name → pod name the create POST handed out
  let podSeq = 0;
  let deletePlan = () => 'ok'; // per pod name: 'ok' | 'fail' | 'hang'
  let createPlan = async () => {};
  let logoutPlan = async () => {};
  let loginPlan = async () => {};
  let mePlan = async () => {};
  let afterBootMe = async () => {};
  let revoked = false;
  let readPlan = async () => answer(403, 'forbidden');
  const pagehide = [];
  const navigation = [];
  let pathname = '/';

  const answer = (status, text) => ({ ok: status >= 200 && status < 300, status,
    text: async () => text, json: async () => JSON.parse(text) });
  // A request that never answers on its own — but honors an abort signal,
  // the way a real fetch dies when the deadline aborts it.
  const hang = (opts = {}) => new Promise((resolve, reject) => {
    const signal = opts.signal;
    if (!signal) return new Promise(() => {}); // no signal, no end
    if (signal.aborted) return reject(new Error('the request was aborted'));
    signal.addEventListener('abort', () => reject(new Error('the request was aborted')), { once: true });
  });
  const fetch = async (url, opts = {}) => {
    const path = String(url);
    const method = opts.method || 'GET';
    calls.push({ method, url: path, init: opts });
    if (path === '/api/v1/config') return answer(200, JSON.stringify({ mode: 'standalone', auth: { required: true } }));
    if (path === '/api/v1/me') {
      const response = cookies.value && revoked ? answer(401, JSON.stringify({
        error: 'unauthenticated', message: 'the cluster no longer accepts this token', csrfToken: cookies.csrf,
      })) : cookies.value ? answer(200, JSON.stringify({
        ...IDENTITY, username: cookies.value === 'initial-session' ? IDENTITY.username : cookies.value, csrfToken: cookies.csrf,
      })) : answer(401, 'session expired');
      await mePlan();
      return response;
    }
    if (path === '/api/v1/login') {
      const token = JSON.parse(opts.body).token;
      await loginPlan(token, opts);
      cookies.value = token;
      cookies.csrf = `${token}-csrf`;
      return answer(200, JSON.stringify({ user: { ...IDENTITY, username: token }, csrfToken: cookies.csrf }));
    }
    if (path === '/api/v1/logout') {
      if (cookies.value && opts.headers['X-Stratum-CSRF'] !== cookies.csrf) return answer(403, 'invalid csrf');
      await logoutPlan(opts);
      // Browsers apply Set-Cookie before fetch resolves, regardless of whether
      // the JavaScript continuation later decides this response is stale.
      cookies.value = null;
      cookies.csrf = null;
      return answer(204, '');
    }
    // A named pod create answers as itself: a name prefix alone must not
    // establish shell ownership ('sign-out leaves a generic prefixed pod
    // alone'). The shell's own create is the batch/v1 Job branch below.
    if (method === 'POST' && path === '/k8s/api/v1/namespaces/default/pods') {
      const body = JSON.parse(opts.body);
      if (body.metadata.name) return answer(201, JSON.stringify({ metadata: { name: body.metadata.name } }));
    }
    // The shell create POSTs a batch/v1 Job; the answer carries the Job's
    // name and the selector its pods will carry, as the apiserver's would.
    if (method === 'POST' && path === '/k8s/apis/batch/v1/namespaces/default/jobs') {
      const body = JSON.parse(opts.body);
      const node = body.metadata.labels['stratum.ciq.com/node-shell'];
      await createPlan(node);
      const name = `stratum-node-shell-${node.replace(/[^a-z0-9]/g, '')}-t${++podSeq}`;
      created[node] = name;
      return answer(201, JSON.stringify({ metadata: { name }, spec: { selector: { matchLabels: { 'job-name': name } } } }));
    }
    if (method === 'DELETE' && path.startsWith('/k8s/apis/batch/v1/namespaces/default/jobs/stratum-node-shell-')) {
      const name = decodeURIComponent(path.split('/jobs/')[1].split('?')[0]);
      const how = deletePlan(name);
      if (how === 'hang') return hang(opts);
      if (how === 'expired') return answer(401, 'session expired');
      if (how === 'fail') return answer(500, 'boom');
      return answer(200, '{}');
    }
    if (method === 'GET' && path.startsWith('/k8s/apis/batch/v1/namespaces/default/jobs/stratum-node-shell-')) {
      if (revoked) return answer(401, 'session expired');
      return answer(200, JSON.stringify({ status: { active: 1 } }));
    }
    // The shell's readiness poll reads the pods the Job made, by the selector
    // the create answer carried; the stub keeps the shell pending forever.
    if (method === 'GET' && path.startsWith('/k8s/api/v1/namespaces/default/pods?labelSelector=')) {
      if (revoked) return answer(401, 'session expired');
      return answer(200, JSON.stringify({ items: [] }));
    }
    if (method === 'GET' && path.startsWith('/k8s/')) return readPlan(path);
    return answer(404, '');
  };

  const context = vm.createContext({
    console: {
      error: (...a) => reports.push(['error', a.map(fmt).join(' ')]),
      warn: (...a) => reports.push(['warn', a.map(fmt).join(' ')]),
      log() {},
    },
    URLSearchParams, AbortController, navigator: { locks },
    __jsx: (type, props, ...children) => ({ type, props: { ...props, children } }),
    __fragment: 'fragment',
    // The bound rides the mockable global timer, so the vm realm must read
    // the test realm's setTimeout when the call is made, never a captured
    // copy — that is what lets t.mock.timers drive the deadline.
    setTimeout: (...a) => globalThis.setTimeout(...a),
    clearTimeout: (...a) => globalThis.clearTimeout(...a),
    fetch,
    window: { localStorage: storage, addEventListener(type, fn) {
      if (type === 'pagehide') pagehide.push(fn);
      if (type === 'storage') storage?.listeners.push(fn);
    } },
  });

  // Run Boot's state and effects when explicitly rendered below. Leaf widgets
  // remain inert; the actual Boot callbacks, session handler and modules run.
  const states = [], effects = [];
  let renderingBoot = false, stateIndex = 0, effectIndex = 0;
  const pendingEffects = [];
  const hooks = {
    useState: (initial) => {
      if (!renderingBoot) return [typeof initial === 'function' ? initial() : initial, () => {}];
      const index = stateIndex++;
      if (!(index in states)) states[index] = typeof initial === 'function' ? initial() : initial;
      return [states[index], (next) => { states[index] = typeof next === 'function' ? next(states[index]) : next; }];
    },
    useEffect: (fn, deps) => {
      if (!renderingBoot) return;
      const index = effectIndex++;
      const previous = effects[index];
      if (!previous || deps.some((value, i) => !Object.is(value, previous.deps[i]))) {
        pendingEffects.push(() => {
          previous?.cleanup?.();
          effects[index] = { deps, cleanup: fn() };
        });
      }
    },
    useRef: () => ({ current: null }), useCallback: (fn) => fn,
    useMemo: (fn) => fn(), useSyncExternalStore: (_subscribe, snapshot) => snapshot(),
  };
  const cache = new Map();
  // The promise is memoized, not the module: the linker resolves one module's
  // imports concurrently, so a cache filled only after the awaits re-enters
  // load for the same specifier and compiles it twice — session.js's session
  // state would fork while the module graph says one instance, and the flow's
  // logout would clear an identity the assertions never read.
  const load = (relative) => {
    if (cache.has(relative)) return cache.get(relative);
    const pending = (async () => {
      let code = await fs.readFile(new URL('../src/' + relative, import.meta.url), 'utf8');
      if (relative.endsWith('.jsx')) {
        ({ code } = await transformWithOxc(code, relative, { jsx: { runtime: 'classic', pragma: '__jsx', pragmaFrag: '__fragment' } }));
      }
      const mod = new vm.SourceTextModule(code, { context, identifier: relative });
      const stub = (values) => new vm.SyntheticModule(Object.keys(values), function () {
        for (const [name, value] of Object.entries(values)) this.setExport(name, value);
      }, { context });
      await mod.link(async (specifier) => {
        // The session and Kubernetes modules run for real — the flow's DELETEs
        // and the logout POST must be observed as the server would see them.
        for (const [match, make] of [
          ['react-router-dom', () => stub({
            BrowserRouter: 'BrowserRouter', useNavigate: () => navigate, useLocation: () => ({ pathname, search: '' }),
            matchPath: () => null, Navigate: 'Navigate', Routes: 'Routes', Route: 'Route',
          })],
          ['react', () => stub(hooks)],
          ['screens/overview.jsx', () => stub({ Overview: 'Overview' })],
          ['screens/infra.jsx', () => stub({ Nodes: 'Nodes', Events: 'Events', Helm: 'Helm' })],
          ['screens/workloads.jsx', () => stub({ Workloads: 'Workloads', KIND_PLURAL: {} })],
          ['screens/tabbed.jsx', () => stub({ TabbedResources: 'TabbedResources', STORAGE_TABS: [], RBAC_TABS: [] })],
          ['screens/login.jsx', () => stub({ Login: 'Login' })],
          ['screens/terminal.jsx', () => stub({ TerminalModal: 'TerminalModal' })],
          ['ui/controls.jsx', () => stub({ Button: 'Button' })],
          ['ui/shell.jsx', () => stub({ AppShell: 'AppShell', Placeholder: 'Placeholder' })],
          ['ui/palette.jsx', () => stub({ CommandPalette: 'CommandPalette' })],
          ['ui/resource-list.jsx', () => stub({ ResourceList: 'ResourceList' })],
          ['ui/exec-logs-drawer.jsx', () => stub({ ExecLogsDrawer: 'ExecLogsDrawer' })],
          ['resources.jsx', () => stub({ RESOURCES: {}, crdDescriptor: () => null })],
          ['data.js', () => load('data.js')],
          ['live.js', () => load('live.js')],
          ['registry.js', () => stub({ getScreen: () => null, screenLabel: () => '' })],
          ['node-shell.js', () => load('node-shell.js')],
          ['k8sutil.js', () => load('k8sutil.js')],
          // k8s-rules.js is k8sutil's re-export source (and live.js's import):
          // real, like the modules above — it is DOM-free and side-effect free.
          ['k8s-rules.js', () => load('k8s-rules.js')],
          ['crd-nav.js', () => load('crd-nav.js')],
          ['session.js', async () => {
            const real = (await load('session.js')).namespace;
            return relative === 'app.jsx' ? stub({ ...real, fetchMe: async () => {
              const who = await real.fetchMe();
              await afterBootMe();
              return who;
            } }) : load('session.js');
          }],
          ['cluster-context.js', () => load('cluster-context.js')],
        ]) {
          if (specifier === match || specifier.endsWith(match)) return make();
        }
        throw new Error('Unmocked dependency in the sign-out loader: ' + specifier);
      });
      await mod.evaluate();
      return mod;
    })();
    cache.set(relative, pending);
    return pending;
  };

  const app = (await load('app.jsx')).namespace;
  const navigate = (path) => { navigation.push(path); pathname = path; };
  const renderBoot = () => {
    const Boot = app.App().props.children[0].type;
    stateIndex = effectIndex = 0;
    renderingBoot = true;
    let tree;
    try { tree = Boot(); } finally { renderingBoot = false; }
    for (const effect of pendingEffects.splice(0)) effect();
    return tree;
  };
  const session = (await load('session.js')).namespace;
  const data = (await load('data.js')).namespace;
  const live = (await load('live.js')).namespace;
  const shells = (await load('node-shell.js')).namespace;
  const { startNodeShell } = shells;
  const k8s = (await load('k8sutil.js')).namespace;
  const startShell = (node) => startNodeShell(node, IMAGE, {
    onPod: () => {}, onReady: () => {}, onError: () => {},
  });
  const deletes = () => calls.filter((c) => c.method === 'DELETE');
  const logouts = () => calls.filter((c) => c.method === 'POST' && c.url === '/api/v1/logout');
  // Boot an existing cookie through /me. After logout, establish a replacement
  // session with the real login helper before reading its identity.
  const signIn = async () => {
    if (!cookies.value) await session.login('next-user');
    await session.fetchMe();
  };
  return { app, session, k8s, shells, data, live, storage, calls, reports, created, startShell, deletes, logouts, signIn, renderBoot, navigation,
    cookie: () => cookies.value,
    pagehide: () => pagehide.forEach((fn) => fn()),
    setDeletePlan: (fn) => { deletePlan = fn; }, setCreatePlan: (fn) => { createPlan = fn; },
    setLogoutPlan: (fn) => { logoutPlan = fn; }, setLoginPlan: (fn) => { loginPlan = fn; },
    setAfterBootMe: (fn) => { afterBootMe = fn; }, setReadPlan: (fn) => { readPlan = fn; }, setMePlan: (fn) => { mePlan = fn; }, revokeToken: () => { revoked = true; } };
}
const fmt = (x) => (typeof x === 'string' ? x : (x && x.message) || String(x));

test('sign-out with no shells open still logs out exactly once and ends signed out', async () => {
  const d = await openDash();
  await d.signIn();

  await bounded(d.app.signOut(), 5000);

  assert.equal(d.logouts().length, 1, 'the logout POST reached the server exactly once');
  assert.equal(d.deletes().length, 0, 'no shells, no deletes');
  assert.equal(d.session.currentIdentity(), null, 'the flow ends signed out');
  assert.equal(d.session.getCsrfToken(), null, 'the session credentials are cleared with it');
});

test('sign-out deletes every open host shell before the logout call', async (t) => {
  // The shell's status poll schedules real 1500ms timers; mocking the clock
  // keeps them off the event loop, so the file settles when the tests do.
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash();
  await d.signIn();
  d.startShell('w-1');
  d.startShell('w-2');
  await settle();

  await bounded(d.app.signOut(), 5000);

  const del = d.deletes();
  const out = d.logouts();
  assert.equal(del.length, 2, 'both open shells get a delete');
  for (const call of del) {
    assert.equal(new URL(call.url, 'https://dashboard.test').searchParams.get('propagationPolicy'), 'Background',
      'sign-out must cascade Job deletion so its privileged pod keeps a cleanup owner');
  }
  const urls = del.map((c) => c.url);
  assert.ok(urls.some((u) => u.startsWith('/k8s/apis/batch/v1/namespaces/default/jobs/' + d.created['w-1'])),
    `w-1's shell Job was not deleted (${urls.join(', ')})`);
  assert.ok(urls.some((u) => u.startsWith('/k8s/apis/batch/v1/namespaces/default/jobs/' + d.created['w-2'])),
    `w-2's shell Job was not deleted (${urls.join(', ')})`);
  assert.equal(out.length, 1, 'the logout POST happened exactly once');
  const logoutAt = d.calls.indexOf(out[0]);
  assert.ok(del.every((c) => d.calls.indexOf(c) < logoutAt),
    'the shell deletes must go out before logout clears the session cookie');
  assert.ok(del.every((c) => c.init.headers['X-Stratum-CSRF'] === 'csrf'),
    'the deletes are writes — each must carry the CSRF header the proxy requires');
  assert.equal(d.session.currentIdentity(), null, 'the flow ends signed out');
});

test('a shell DELETE that hangs is abandoned at the bound and logout proceeds at the bound', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash();
  await d.signIn();
  d.startShell('w-1');
  await settle();
  d.setDeletePlan(() => 'hang');

  const done = d.app.signOut({ deadlineMs: 50 });
  await settle();
  assert.equal(d.deletes().length, 1, 'the delete was attempted');
  assert.equal(d.logouts().length, 0, 'a hung delete holds the sign-out attempt open');

  t.mock.timers.tick(49);
  await settle();
  assert.equal(d.logouts().length, 0, 'still inside the bound — logout has not run');

  t.mock.timers.tick(100); // past the bound, still far short of the transport's 15s deadline
  await bounded(done, 5000);
  assert.equal(d.logouts().length, 1, 'logout ran at the bound — the hung delete did not stop sign-out');
  assert.ok(d.calls.indexOf(d.deletes()[0]) < d.calls.indexOf(d.logouts()[0]),
    'even the abandoned delete went out before logout');
  assert.equal(d.deletes().length, 1, 'the hung delete is abandoned, not retried');
});

test('a shell DELETE that fails is reported per shell, and sign-out still completes', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] }); // the status poll never touches the real clock
  const d = await openDash();
  await d.signIn();
  d.startShell('w-1');
  d.startShell('w-2');
  await settle();
  d.setDeletePlan((name) => (name === d.created['w-1'] ? 'fail' : 'ok'));

  await bounded(d.app.signOut(), 5000); // resolves — a failed delete must not prevent signing out

  assert.equal(d.deletes().length, 2, 'one failure does not skip the other shell');
  assert.equal(d.logouts().length, 1, 'logout happened exactly once after the failure');
  const report = d.reports.map(([channel, text]) => text).join('\n');
  assert.ok(report.includes(d.created['w-1']) || report.includes('w-1'),
    `the failed shell must be named in a report, not swallowed (${report || 'nothing was reported'})`);
  assert.equal(d.session.currentIdentity(), null, 'the flow still ends signed out');
});

test('sign-out waits for an in-flight shell create and deletes it before logout', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash();
  await d.signIn();
  let release;
  d.setCreatePlan(() => new Promise((resolve) => { release = resolve; }));
  d.startShell('w-1');
  await settle();
  const done = d.app.signOut({ deadlineMs: 100 });
  await settle();
  assert.equal(d.logouts().length, 0, 'logout must wait while a shell create can still complete');
  release();
  await bounded(done, 5000);
  assert.equal(d.deletes().length, 1);
  assert.ok(d.calls.indexOf(d.deletes()[0]) < d.calls.indexOf(d.logouts()[0]));
});

test('a shell create that never answers cannot hold sign-out indefinitely', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash();
  await d.signIn();
  d.setCreatePlan(() => new Promise(() => {}));
  d.startShell('w-1');
  const done = d.app.signOut({ deadlineMs: 50 });
  await settle();
  assert.equal(d.logouts().length, 0);
  t.mock.timers.tick(50);
  await bounded(done, 5000);
  assert.equal(d.logouts().length, 1);
  assert.ok(d.reports.some(([channel, message]) => channel === 'error' && message.includes('create did not finish')));
});

test('successful sign-out retires shells before component cleanup can delete them again', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash();
  await d.signIn();
  const stop = d.startShell('w-1');
  await settle();
  await d.app.signOut();
  assert.equal(d.shells.openNodeShells().length, 0);
  stop();
  d.pagehide();
  await settle();
  assert.equal(d.deletes().length, 1, 'unmount and pagehide must not repeat an accepted delete');
});

test('failed shell cleanup cannot retry under the next session', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash();
  await d.signIn();
  const stop = d.startShell('w-1');
  await settle();
  d.setDeletePlan(() => 'fail');
  await d.app.signOut();
  assert.equal(d.deletes().length, 1);
  await d.signIn();
  assert.equal(d.session.currentIdentity().username, 'next-user');
  assert.equal(d.cookie(), 'next-user');
  d.session.setCsrfToken('next-user-csrf');
  d.setDeletePlan(() => 'ok');
  stop();
  d.pagehide();
  await d.app.signOut();
  await settle();
  assert.equal(d.deletes().length, 1, 'another session must not inherit prior cleanup authority');
});

test('a shell create completing after logout cannot register or delete under the next session', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash();
  await d.signIn();
  let release;
  d.setCreatePlan(() => new Promise((resolve) => { release = resolve; }));
  const stop = d.startShell('w-1');
  const done = d.app.signOut({ deadlineMs: 50 });
  await settle();
  t.mock.timers.tick(50);
  await bounded(done, 5000);
  await d.signIn();
  assert.equal(d.session.currentIdentity().username, 'next-user');
  assert.equal(d.cookie(), 'next-user');
  d.session.setCsrfToken('next-user-csrf');
  release();
  await settle();
  stop();
  d.pagehide();
  await d.app.signOut();
  await settle();
  assert.equal(d.shells.openNodeShells().length, 0);
  assert.equal(d.deletes().length, 0, 'late create continuation must not use the next session');
});

test('sign-out leaves a generic prefixed pod alone', async () => {
  const d = await openDash();
  await d.signIn();
  await d.k8s.k8sPost('/api/v1/namespaces/default/pods',
    { metadata: { name: 'stratum-node-shell-user-owned' }, spec: {} });
  await settle();
  await d.app.signOut();
  assert.equal(d.deletes().length, 0, 'a name prefix alone does not establish shell ownership');
});

test('sign-out and pagehide leave a generic prefixed Job alone', async () => {
  const d = await openDash();
  await d.signIn();
  // The Job mirror of the pod test above: a stratum-node-shell-* Job this
  // browser session never created — the one an earlier session's tab or
  // another editor left behind, still inside its ttlSecondsAfterFinished
  // window. POSTed straight through k8sutil, it reaches neither the owned
  // registry nor a pending create: ownership comes only from the SPA's own
  // create POST answering into startNodeShell, so only registry-owned names
  // are ever deleted, and nothing else (a name prefix, an existing object)
  // establishes it.
  const foreign = await d.k8s.k8sPost('/apis/batch/v1/namespaces/default/jobs', {
    apiVersion: 'batch/v1', kind: 'Job',
    metadata: { generateName: 'stratum-node-shell-', labels: { app: 'stratum-node-shell', 'stratum.ciq.com/node-shell': 'w-foreign' } },
    spec: { ttlSecondsAfterFinished: 600, activeDeadlineSeconds: 3600, backoffLimit: 0, template: { spec: { hostPID: true } } },
  });
  assert.ok(foreign.ok, 'the fake API accepted the foreign Job create');
  const name = foreign.body.metadata.name;
  assert.ok(name.startsWith('stratum-node-shell-'), 'the foreign Job carries the shell name prefix');
  assert.deepEqual(Array.from(d.shells.openNodeShells(), (shell) => shell.pod), [],
    'the foreign Job is in no registry the cleanup reads');
  assert.equal(d.shells.pendingNodeShellCreates().length, 0, 'and it is no pending create either');
  d.pagehide();
  await settle();
  await d.app.signOut();
  assert.equal(d.logouts().length, 1, 'the sign-out flow itself ran to completion');
  assert.equal(d.deletes().length, 0,
    'a name prefix alone does not establish shell ownership — the foreign Job got no DELETE from pagehide or sign-out');
  const still = await d.k8s.k8sGet('/apis/batch/v1/namespaces/default/jobs/' + name, null);
  assert.equal(still && still.status && still.status.active, 1, 'the foreign Job still stands, its TTL window untouched');
});

const shellProps = (tree) => {
  assert.equal(tree.type, 'Routes', 'Boot must keep the authenticated shell ready');
  return tree.props.children.find((route) => route.props.path === '*').props.element.props;
};
const bootSignedIn = async (d) => {
  d.renderBoot();
  await settle();
  return shellProps(d.renderBoot());
};
const signInNextSession = async (d) => {
  const screen = d.renderBoot();
  assert.equal(screen.type, 'Login', 'session expiry exposed sign-in');
  const result = await d.session.login('user-b');
  await screen.props.onSignedIn(result.body.user);
  assert.equal(shellProps(d.renderBoot()).me.username, 'user-b');
};

test('a superseded sign-out cannot log out the next user or reset their shell and UI', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash();
  const initial = await bootSignedIn(d);
  d.startShell('old-1');
  d.startShell('old-2');
  await settle();
  d.setDeletePlan((pod) => pod === d.created['old-1'] ? 'expired' : 'hang');
  const done = initial.onSignOut();
  try {
    await settle();
    assert.equal(d.deletes().length, 2);
    assert.equal(d.logouts().length, 0, 'the second cleanup request is still pending');
    // Expiry from an ordinary request can still expose a replacement login
    // while explicit sign-out is waiting for its bounded cleanup.
    d.session.notifyUnauthorized('/k8s/api/v1/pods');
    await signInNextSession(d);
    d.startShell('new');
    await settle();
    const navigationBefore = d.navigation.length;
    t.mock.timers.tick(3000);
    await bounded(done, 5000);
    assert.equal(d.logouts().length, 0, 'the obsolete cleanup must not send logout with the new cookie');
    assert.equal(d.session.currentIdentity().username, 'user-b');
    assert.equal(d.session.getCsrfToken(), 'user-b-csrf');
    assert.equal(shellProps(d.renderBoot()).me.username, 'user-b', 'Boot keeps the new session ready');
    assert.equal(d.navigation.length, navigationBefore, 'the old continuation must not navigate to login');
    assert.deepEqual(Array.from(d.shells.openNodeShells(), (shell) => shell.pod), [d.created.new],
      'the obsolete cleanup must not retire the new user\'s shell');
  } finally {
    t.mock.timers.tick(3000);
    await bounded(done, 5000);
    d.session.clearSession();
    d.shells.endNodeShellSession();
  }
});

test('replacement login waits for pending logout cookie deletion and keeps Boot ready', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash();
  const initial = await bootSignedIn(d);
  let release;
  d.setLogoutPlan(() => new Promise((resolve) => { release = resolve; }));
  const done = initial.onSignOut();
  let signingIn;
  try {
    await settle();
    assert.equal(d.logouts().length, 1);
    d.session.notifyUnauthorized('/k8s/api/v1/pods');
    signingIn = signInNextSession(d);
    await settle();
    assert.equal(d.calls.filter((call) => call.url === '/api/v1/login').length, 0,
      'a new cookie must not be issued before the old logout response clears it');
    const navigationBefore = d.navigation.length;
    release();
    await bounded(signingIn, 5000);
    await bounded(done, 5000);
    assert.equal(d.cookie(), 'user-b', 'the logout response must not erase the new browser session');
    assert.equal(d.session.currentIdentity().username, 'user-b');
    assert.equal(d.session.getCsrfToken(), 'user-b-csrf');
    assert.equal(shellProps(d.renderBoot()).me.username, 'user-b', 'Boot ignores the obsolete logout completion');
    assert.equal(d.navigation.length, navigationBefore);
    assert.equal(d.logouts().length, 1, 'the stale completion must not issue another logout');
    assert.equal((await d.session.fetchMe()).username, 'user-b', 'the next authenticated request still succeeds');
  } finally {
    release?.();
    if (signingIn) await bounded(signingIn, 5000);
    await bounded(done, 5000);
    d.session.clearSession();
    d.shells.endNodeShellSession();
  }
});

test('sign-out waiting for an old create cannot clean up a new session', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash();
  const initial = await bootSignedIn(d);
  let release;
  d.setCreatePlan((node) => node === 'old' ? new Promise((resolve) => { release = resolve; }) : Promise.resolve());
  d.startShell('old');
  const done = initial.onSignOut();
  try {
    await settle();
    assert.equal(d.logouts().length, 0);
    d.session.notifyUnauthorized('/k8s/api/v1/pods');
    await signInNextSession(d);
    d.startShell('new');
    await settle();
    release();
    await bounded(done, 5000);
    assert.equal(d.deletes().length, 0, 'the old sign-out must not delete shells registered by the new session');
    assert.equal(d.logouts().length, 0);
    assert.equal(d.session.currentIdentity().username, 'user-b');
    assert.equal(shellProps(d.renderBoot()).me.username, 'user-b');
    assert.deepEqual(Array.from(d.shells.openNodeShells(), (shell) => shell.pod), [d.created.new]);
  } finally {
    release?.();
    await bounded(done, 5000);
    d.session.clearSession();
    d.shells.endNodeShellSession();
  }
});

test('a hung logout is aborted so replacement login can proceed at the deadline', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash();
  const initial = await bootSignedIn(d);
  let release;
  d.setLogoutPlan((opts) => new Promise((resolve, reject) => {
    release = resolve;
    opts.signal?.addEventListener('abort', () => reject(new Error('logout aborted')), { once: true });
  }));
  const done = initial.onSignOut();
  let signingIn;
  try {
    await settle();
    const request = d.logouts()[0];
    d.session.notifyUnauthorized('/k8s/api/v1/pods');
    signingIn = signInNextSession(d);
    await settle();
    t.mock.timers.tick(14999);
    await settle();
    assert.equal(d.calls.filter((call) => call.url === '/api/v1/login').length, 0);
    t.mock.timers.tick(1);
    await bounded(Promise.all([done, signingIn]), 5000);
    assert.equal(request.init.signal.aborted, true, 'the old request cannot deliver a later cookie deletion');
    assert.equal(d.cookie(), 'user-b');
    assert.equal((await d.session.fetchMe()).username, 'user-b');
    assert.equal(shellProps(d.renderBoot()).me.username, 'user-b');
  } finally {
    release?.();
    if (signingIn) await bounded(signingIn, 5000);
    await bounded(done, 5000);
    d.session.clearSession();
    d.shells.endNodeShellSession();
  }
});

test('concurrent login responses preserve the latest cookie, identity, and CSRF', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash();
  let release;
  d.setLoginPlan((token) => token === 'user-a' ? new Promise((resolve) => { release = resolve; }) : Promise.resolve());
  const first = d.session.login('user-a');
  await settle();
  const second = d.session.login('user-b');
  try {
    await settle();
    assert.equal(d.calls.filter((call) => call.url === '/api/v1/login').length, 1,
      'replacement login must wait for the previous cookie-setting response');
    release();
    const [old, latest] = await bounded(Promise.all([first, second]), 5000);
    assert.equal(old.stale, true);
    assert.equal(latest.ok, true);
    assert.equal(d.cookie(), 'user-b');
    assert.equal(d.session.currentIdentity().username, 'user-b');
    assert.equal(d.session.getCsrfToken(), 'user-b-csrf');
    assert.equal((await d.session.fetchMe()).username, 'user-b');
  } finally {
    release?.();
    await bounded(Promise.all([first, second]), 5000);
    d.session.clearSession();
  }
});

test('a hung login is aborted and superseded queued logins never issue cookies', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash();
  let release;
  d.setLoginPlan((token, opts) => token === 'user-a' ? new Promise((resolve, reject) => {
    release = resolve;
    opts.signal?.addEventListener('abort', () => reject(new Error('login aborted')), { once: true });
  }) : Promise.resolve());
  const first = d.session.login('user-a');
  await settle();
  const second = d.session.login('user-b');
  const third = d.session.login('user-c');
  try {
    await settle();
    t.mock.timers.tick(14999);
    await settle();
    assert.equal(d.calls.filter((call) => call.url === '/api/v1/login').length, 1);
    t.mock.timers.tick(1);
    const [old, skipped, latest] = await bounded(Promise.all([first, second, third]), 5000);
    assert.equal(old.stale, true);
    assert.equal(skipped.code, 'superseded');
    assert.equal(latest.ok, true);
    assert.deepEqual(d.calls.filter((call) => call.url === '/api/v1/login').map((call) => JSON.parse(call.init.body).token),
      ['user-a', 'user-c']);
    assert.equal(d.cookie(), 'user-c');
    assert.equal(d.session.currentIdentity().username, 'user-c');
    assert.equal(d.session.getCsrfToken(), 'user-c-csrf');
  } finally {
    release?.();
    await bounded(Promise.all([first, second, third]), 5000);
    d.session.clearSession();
  }
});

test('logout waits for an in-flight login cookie and then clears that session', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash();
  let release;
  d.setLoginPlan(() => new Promise((resolve) => { release = resolve; }));
  const signingIn = d.session.login('user-a');
  await settle();
  const signingOut = d.session.logout();
  try {
    await settle();
    assert.equal(d.logouts().length, 0, 'logout must follow the outstanding cookie-setting response');
    release();
    const [, result] = await bounded(Promise.all([signingIn, signingOut]), 5000);
    assert.equal(result.ok, true, 'logout must use the CSRF token associated with the preceding login cookie');
    assert.equal(d.cookie(), null);
    assert.equal(d.session.currentIdentity(), null);
    assert.equal(d.session.getCsrfToken(), null);
  } finally {
    release?.();
    await bounded(Promise.all([signingIn, signingOut]), 5000);
    d.session.clearSession();
  }
});

test('an old-cookie session probe cannot replace the CSRF used by queued logout', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  for (const probe of ['fetchMe', 'probeMe']) {
    const d = await openDash();
    await d.signIn();
    let releaseLogin, releaseMe;
    d.setLoginPlan(() => new Promise((resolve) => { releaseLogin = resolve; }));
    const signingIn = d.session.login('user-a');
    await settle();
    const signingOut = d.session.logout();
    d.setMePlan(() => new Promise((resolve) => { releaseMe = resolve; }));
    const checking = d.session[probe]();
    try {
      await settle();
      releaseLogin();
      releaseMe();
      const [, result] = await bounded(Promise.all([signingIn, signingOut, checking]), 5000);
      assert.equal(result.ok, true, `${probe} must not replace the preceding login cookie's CSRF`);
      assert.equal(d.cookie(), null, `${probe} must not leave a valid cookie after logout`);
      assert.equal(d.session.currentIdentity(), null);
      assert.equal(d.session.getCsrfToken(), null);
    } finally {
      releaseLogin?.();
      releaseMe?.();
      await bounded(Promise.all([signingIn, signingOut, checking]), 5000);
      d.session.clearSession();
    }
  }
});

// Separate module graphs represent separate documents. They share the browser
// cookie store and Node's real Web Lock manager, not session.js's module state.
const sharedStorage = () => ({
  value: null, listeners: [], failRead: false, failWrite: false,
  getItem() { if (this.failRead) throw new Error('storage unavailable'); return this.value; },
  setItem(_key, value) { if (this.failWrite) throw new Error('storage unavailable'); this.value = value; },
  // Delay events independently of the synchronous store, like a background tab.
  flush() { for (const fn of this.listeners) fn({ key: 'stratum-dashboard-session-generation' }); },
});
const twoTabs = async () => {
  const options = { cookies: { value: 'initial-session', csrf: 'csrf' }, locks: navigator.locks, storage: sharedStorage() };
  return Promise.all([openDash(options), openDash(options)]);
};

test('a login in another document waits for the earlier cookie-changing response', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const [a, b] = await twoTabs();
  let started, release;
  const entered = new Promise((resolve) => { started = resolve; });
  a.setLoginPlan(() => new Promise((resolve) => { release = resolve; started(); }));
  const first = a.session.login('user-a');
  await bounded(entered, 5000);
  const second = b.session.login('user-b');
  try {
    await settle();
    assert.equal(b.calls.filter((call) => call.url === '/api/v1/login').length, 0,
      'a different module graph must share the cookie lock');
    release();
    await bounded(Promise.all([first, second]), 5000);
    assert.equal(b.cookie(), 'user-b');
    assert.equal(b.session.currentIdentity().username, 'user-b');
    assert.equal(b.session.getCsrfToken(), 'user-b-csrf');
    assert.equal((await b.session.fetchMe()).username, 'user-b');
  } finally {
    release?.();
    await bounded(Promise.all([first, second]), 5000);
    a.session.clearSession(); b.session.clearSession();
  }
});

test('a delayed logout in another document cannot erase a replacement login cookie', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const [a, b] = await twoTabs();
  await a.signIn();
  let started, release;
  const entered = new Promise((resolve) => { started = resolve; });
  a.setLogoutPlan(() => new Promise((resolve) => { release = resolve; started(); }));
  const first = a.session.logout();
  await bounded(entered, 5000);
  const second = b.session.login('user-b');
  try {
    await settle();
    assert.equal(b.calls.filter((call) => call.url === '/api/v1/login').length, 0);
    release();
    await bounded(Promise.all([first, second]), 5000);
    assert.equal(b.cookie(), 'user-b');
    assert.equal(b.session.currentIdentity().username, 'user-b');
    assert.equal((await b.session.fetchMe()).username, 'user-b');
  } finally {
    release?.();
    await bounded(Promise.all([first, second]), 5000);
    a.session.clearSession(); b.session.clearSession();
  }
});

test('logout refreshes its CSRF under the lock after another document changes the cookie', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const [a, b] = await twoTabs();
  await b.signIn();
  try {
    await a.session.login('user-a');
    assert.equal(b.session.getCsrfToken(), null, 'the other document must discard its original UI token');
    const result = await b.session.logout();
    assert.equal(result.ok, true, 'logout must use the current shared cookie\'s CSRF token');
    assert.equal(b.logouts()[0].init.headers['X-Stratum-CSRF'], 'user-a-csrf');
    assert.equal(b.cookie(), null);
    assert.equal(b.session.currentIdentity(), null);
  } finally {
    a.session.clearSession(); b.session.clearSession();
  }
});

test('a login superseded while waiting for another document never issues a cookie', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const [a, b] = await twoTabs();
  let started, release;
  const entered = new Promise((resolve) => { started = resolve; });
  a.setLoginPlan(() => new Promise((resolve) => { release = resolve; started(); }));
  const first = a.session.login('user-a');
  await bounded(entered, 5000);
  const second = b.session.login('user-b');
  await settle();
  const third = b.session.logout();
  try {
    release();
    const [, skipped, result] = await bounded(Promise.all([first, second, third]), 5000);
    assert.equal(skipped.code, 'superseded');
    assert.equal(b.calls.filter((call) => call.url === '/api/v1/login').length, 0);
    assert.equal(result.ok, true);
    assert.equal(b.cookie(), null);
  } finally {
    release?.();
    await bounded(Promise.all([first, second, third]), 5000);
    a.session.clearSession(); b.session.clearSession();
  }
});

test('a rejected Web Lock returns an error and never bypasses cross-document coordination', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash({ locks: { request: async () => { throw new Error('lock unavailable'); } } });
  try {
    const result = await d.session.login('user-a');
    assert.equal(result.ok, false);
    assert.equal(result.status, 0);
    assert.match(result.error, /lock unavailable/);
    assert.equal(d.calls.filter((call) => call.url === '/api/v1/login').length, 0);
    assert.equal(d.cookie(), 'initial-session');
  } finally { d.session.clearSession(); }
});

test('a hung request releases the shared lock at its deadline so another document can sign in', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const [a, b] = await twoTabs();
  let started, release;
  const entered = new Promise((resolve) => { started = resolve; });
  a.setLoginPlan((_token, opts) => new Promise((resolve, reject) => {
    release = resolve;
    opts.signal.addEventListener('abort', () => reject(new Error('login aborted')), { once: true });
    started();
  }));
  const first = a.session.login('user-a');
  await bounded(entered, 5000);
  // The waiter arrives later, so its acquisition budget extends beyond the
  // owner's active deadline instead of expiring at that same clock tick.
  t.mock.timers.tick(5000);
  const second = b.session.login('user-b');
  try {
    t.mock.timers.tick(9999);
    await settle();
    assert.equal(b.calls.filter((call) => call.url === '/api/v1/login').length, 0);
    t.mock.timers.tick(1);
    const [expired, result] = await bounded(Promise.all([first, second]), 5000);
    assert.equal(expired.ok, false);
    assert.equal(result.ok, true);
    assert.equal(b.cookie(), 'user-b');
    assert.equal(b.session.currentIdentity().username, 'user-b');
    assert.equal(b.session.getCsrfToken(), 'user-b-csrf');
  } finally {
    release?.();
    await bounded(Promise.all([first, second]), 5000);
    a.session.clearSession(); b.session.clearSession();
  }
});

test('coordinated logout clears a valid cookie even when its Kubernetes token was revoked', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const [a, b] = await twoTabs();
  await b.signIn();
  try {
    await a.session.login('user-a');
    b.revokeToken();
    assert.equal(await b.session.fetchMe(), null, 'the rejected token must not authenticate the UI');
    const result = await b.session.logout();
    assert.equal(result.ok, true, 'explicit logout can still clear the sealed dashboard session');
    assert.equal(b.logouts()[0].init.headers['X-Stratum-CSRF'], 'user-a-csrf');
    assert.equal(b.cookie(), null);
    assert.equal(b.session.currentIdentity(), null);
    assert.equal(b.session.getCsrfToken(), null);
  } finally { a.session.clearSession(); b.session.clearSession(); }
});

test('waiting for a shared lock has a deadline and never bypasses its current owner', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let entered, release;
  const ready = new Promise((resolve) => { entered = resolve; });
  const held = navigator.locks.request('stratum-dashboard-session', () => new Promise((resolve) => {
    release = resolve; entered();
  }));
  await bounded(ready, 5000);
  const d = await openDash({ locks: navigator.locks });
  const pending = d.session.login('user-b');
  try {
    await settle();
    t.mock.timers.tick(14999);
    await settle();
    assert.equal(d.calls.length, 0);
    t.mock.timers.tick(1);
    const result = await bounded(pending, 5000);
    assert.equal(result.ok, false);
    assert.equal(result.status, 0);
    assert.equal(d.calls.length, 0, 'the failed lock acquisition must not send an uncoordinated request');
    assert.equal(d.cookie(), 'initial-session');
  } finally {
    release?.();
    await bounded(Promise.all([held, pending]), 5000);
    d.session.clearSession();
  }
});

test('another tab login invalidates delayed identity probes before storage events arrive', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  for (const probe of ['fetchMe', 'probeMe']) {
    const [a, b] = await twoTabs();
    await a.signIn();
    a.data.K8S.PODS = [{ name: 'private-a' }];
    let release;
    a.setMePlan(() => new Promise((resolve) => { release = resolve; }));
    const pending = a.session[probe]();
    try {
      await settle();
      assert.equal((await b.session.login('user-b')).ok, true);
      release();
      const result = await bounded(pending, 5000);
      assert.ok(probe === 'fetchMe' ? result === null : result.stale);
      assert.equal(a.session.currentIdentity(), null);
      assert.equal(a.session.getCsrfToken(), null);
      assert.equal(a.data.K8S.PODS.length, 0);
      await a.live.loadLiveK8S();
      assert.equal(a.data.K8S.PODS.length, 0, 'B forbidden reads cannot preserve A private rows');
      assert.equal(b.session.currentIdentity().username, 'user-b');
    } finally { release?.(); await bounded(pending, 5000); }
  }
});

test('a delayed snapshot cannot repopulate private rows after another tab login', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const [a, b] = await twoTabs();
  await a.signIn();
  let release;
  const gate = new Promise((resolve) => { release = resolve; });
  a.setReadPlan(async () => { await gate; return { ok: true, status: 200,
    text: async () => JSON.stringify({ items: [{ metadata: { name: 'private-a', namespace: 'default' } }] }) }; });
  const pending = a.live.loadLiveK8S();
  try {
    await settle();
    assert.equal((await b.session.login('user-b')).ok, true);
    release();
    const result = await bounded(pending, 5000);
    assert.equal(result.superseded, true);
    assert.equal(a.data.K8S.PODS.length, 0);
    assert.equal(a.data.K8S.WORKLOADS.length, 0);
  } finally { release(); await bounded(pending, 5000); }
});

test('remote session change removes shell cleanup authority and returns Boot to login', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const [a, b] = await twoTabs();
  a.renderBoot(); await settle(); a.renderBoot();
  const stop = a.startShell('w-1'); await settle();
  assert.equal(a.shells.openNodeShells().length, 1);
  await b.session.login('user-b');
  a.storage.flush();
  assert.equal(a.shells.openNodeShells().length, 0);
  stop(); a.pagehide(); await settle();
  assert.equal(a.deletes().length, 0, 'old callbacks cannot delete under B credentials');
  assert.equal(a.renderBoot().type, 'Login');
  assert.equal(a.navigation.at(-1), '/login');
  assert.equal(b.session.currentIdentity().username, 'user-b', 'late own events must preserve the writer');
});

test('private snapshots are refused while another tab is changing the cookie', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const [a, b] = await twoTabs();
  await a.signIn();
  let started, release;
  const entered = new Promise((resolve) => { started = resolve; });
  b.setLoginPlan(() => new Promise((resolve) => { release = resolve; started(); }));
  const pending = b.session.login('user-b');
  try {
    await bounded(entered, 5000);
    assert.equal(await a.session.fetchMe(), null, 'old cookie cannot authenticate during mutation');
    const outcome = await a.live.loadLiveK8S();
    assert.equal(outcome.superseded, true);
    assert.equal(a.session.currentIdentity(), null);
  } finally { release?.(); await bounded(pending, 5000); }
});

test('unavailable shared storage prevents cookie mutation and discards cached private data', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const [a, b] = await twoTabs();
  await a.signIn();
  a.data.K8S.PODS = [{ name: 'private-a' }];
  a.storage.failRead = true;
  assert.equal(a.session.currentIdentity(), null);
  assert.equal(a.data.K8S.PODS.length, 0);
  const result = await b.session.login('user-b');
  assert.equal(result.ok, false);
  assert.equal(b.calls.filter((c) => c.url === '/api/v1/login').length, 0);
  assert.equal(b.cookie(), 'initial-session');
  a.storage.failRead = false;
  a.storage.failWrite = true;
  assert.equal((await b.session.login('user-b')).ok, false);
  assert.equal(b.calls.filter((c) => c.url === '/api/v1/login').length, 0);
});

test('a completed login cannot restore identity after another document has replaced its cookie', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const cookies = { value: 'initial-session', csrf: 'csrf' }, storage = sharedStorage();
  let completed, release;
  const ready = new Promise((resolve) => { completed = resolve; });
  const a = await openDash({ cookies, storage, locks: { request: async (...args) => {
    const result = await navigator.locks.request(...args);
    completed();
    await new Promise((resolve) => { release = resolve; });
    return result;
  } } });
  const b = await openDash({ cookies, storage, locks: navigator.locks });
  const first = a.session.login('user-a');
  try {
    await bounded(ready, 5000);
    assert.equal((await b.session.login('user-b')).ok, true);
    release();
    const result = await bounded(first, 5000);
    assert.equal(result.stale, true);
    assert.equal(result.ok, false, 'obsolete success must never enter Login success handling');
    assert.equal(a.session.currentIdentity(), null);
    assert.equal(b.session.currentIdentity().username, 'user-b');
  } finally { release?.(); await bounded(first, 5000); }
});

test('Boot refuses a login result whose cookie changed before the UI continuation', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const [a, b] = await twoTabs();
  a.renderBoot(); await settle(); a.renderBoot();
  a.session.notifyUnauthorized('/k8s/api/v1/pods');
  const screen = a.renderBoot();
  assert.equal(screen.type, 'Login');
  const result = await a.session.login('user-a');
  assert.equal(result.ok, true);
  await b.session.login('user-b');
  await screen.props.onSignedIn(result.body.user, result.body.csrfToken, result.revision);
  assert.equal(a.renderBoot().type, 'Login', 'stale UI continuation cannot render an authenticated shell');
  assert.equal(a.session.currentIdentity(), null);
});

test('failed completion publication keeps private reads fenced until a later login recovers', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const [a, b] = await twoTabs();
  await a.signIn();
  b.setLoginPlan(async () => { b.storage.failWrite = true; });
  const result = await b.session.login('user-b');
  assert.equal(result.ok, false);
  assert.equal(b.cookie(), 'user-b', 'the browser already applied Set-Cookie');
  assert.equal(b.session.currentIdentity(), null, 'the writer cannot retain the previous identity');
  assert.equal(await a.session.fetchMe(), null);
  assert.equal((await a.live.loadLiveK8S()).superseded, true);
  assert.equal(a.session.currentIdentity(), null);
  b.storage.failWrite = false;
  b.setLoginPlan(async () => {});
  assert.equal((await b.session.login('user-c')).ok, true);
  assert.equal((await a.session.fetchMe()).username, 'user-c');
  assert.equal(b.session.currentIdentity().username, 'user-c');
});


test('a local replacement login retires old shell callbacks without relying on React cleanup', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash();
  await d.signIn();
  const stop = d.startShell('w-1'); await settle();
  assert.equal(d.shells.openNodeShells().length, 1);
  assert.equal((await d.session.login('user-b')).ok, true);
  stop(); d.pagehide(); await settle();
  assert.equal(d.shells.openNodeShells().length, 0);
  assert.equal(d.deletes().length, 0);
});


test('initial Boot cannot publish identity if another tab changes its cookie after the probe settles', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const [a, b] = await twoTabs();
  let probed, release;
  const ready = new Promise((resolve) => { probed = resolve; });
  a.setAfterBootMe(() => new Promise((resolve) => { release = resolve; probed(); }));
  a.renderBoot();
  try {
    await bounded(ready, 5000);
    assert.equal((await b.session.login('user-b')).ok, true);
    release(); await settle();
    const tree = a.renderBoot();
    assert.equal(tree.type, 'Navigate');
    assert.equal(tree.props.to, '/login');
    assert.equal(a.session.currentIdentity(), null);
    assert.equal(b.session.currentIdentity().username, 'user-b');
  } finally { release?.(); await settle(); }
});

test('sign-out clears a revoked sealed cookie after reporting a shell cleanup 401', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  for (const locks of [undefined, navigator.locks]) {
    const d = await openDash({ locks });
    const initial = await bootSignedIn(d);
    d.startShell('w-1'); await settle();
    d.revokeToken();
    d.setDeletePlan(() => 'expired');
    await bounded(initial.onSignOut(), 5000);
    assert.equal(d.deletes().length, 1);
    assert.ok(d.reports.some(([level, message]) => level === 'error' && message.includes('host shell delete failed')));
    assert.equal(d.logouts().length, 1, 'failed cleanup must not suppress explicit logout');
    assert.equal(d.cookie(), null, 'explicit logout clears the sealed cookie despite revoked cluster credentials');
    assert.equal(d.session.currentIdentity(), null);
    assert.equal(d.session.getCsrfToken(), null);
    assert.equal(d.shells.openNodeShells().length, 0);
    assert.equal(d.renderBoot().type, 'Login');
  }
});

test('logout without Web Locks recovers cookie CSRF after ordinary expiry erased UI credentials', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash();
  await bootSignedIn(d);
  d.revokeToken();
  d.setDeletePlan(() => 'expired');
  const expired = await d.k8s.k8sFetch('DELETE', '/apis/batch/v1/namespaces/default/jobs/stratum-node-shell-expired');
  assert.equal(expired.status, 401);
  assert.equal(d.session.currentIdentity(), null, 'ordinary transport 401 still routes expiry');
  assert.equal(d.session.getCsrfToken(), null);
  assert.equal(d.cookie(), 'initial-session', 'expiry has not yet deleted the sealed cookie');
  const result = await bounded(d.session.logout(), 5000);
  assert.equal(result.ok, true);
  assert.equal(d.logouts()[0].init.headers['X-Stratum-CSRF'], 'csrf');
  assert.equal(d.cookie(), null);
});

test('ordinary expiry during bounded shell cleanup still permits explicit logout when no login replaced it', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const d = await openDash();
  const initial = await bootSignedIn(d);
  d.startShell('w-1'); await settle();
  d.setDeletePlan(() => 'hang');
  const signingOut = initial.onSignOut();
  try {
    await settle();
    assert.equal(d.deletes().length, 1);
    d.revokeToken();
    const expired = await d.k8s.k8sFetch('GET', '/apis/batch/v1/namespaces/default/jobs/' + d.created['w-1']);
    assert.equal(expired.status, 401);
    assert.equal(d.session.currentIdentity(), null);
    assert.equal(d.cookie(), 'initial-session');
    t.mock.timers.tick(3000);
    await bounded(signingOut, 5000);
    assert.equal(d.logouts().length, 1);
    assert.equal(d.cookie(), null, 'expiry alone must not be mistaken for a replacement login');
  } finally {
    t.mock.timers.tick(3000);
    await bounded(signingOut, 5000);
  }
});
