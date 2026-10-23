import assert from 'node:assert/strict';
import test from 'node:test';

import { harness, collect } from './harness.mjs';
import { nodeDrift } from '../src/version.js';

// live.js talks to the apiserver through the global fetch and stamps window on
// success, so both have to exist before it is imported.
globalThis.window = {};
// This fixture exercises the document-local fallback. Cross-document session
// coordination runs against the native LockManager in signout.test.mjs.
Object.defineProperty(globalThis, 'navigator', { value: {}, configurable: true });
const api = { routes: {}, trace: [], unbounded: 0, calls: [] };
globalThis.fetch = async (url, opts) => {
  // Every request must carry a deadline: the next poll is only scheduled after
  // the current load settles, so one request that never returns would stop the
  // dashboard refreshing for good.
  if (!opts || !opts.signal) api.unbounded += 1;
  api.calls.push({ url: String(url), accept: opts && opts.headers && opts.headers.Accept });
  const path = String(url).replace(/^\/k8s/, '');
  // /version opens a load and the operator lease closes it, so the trace shows
  // whether two loads ran back to back or on top of each other.
  if (path === '/version') api.trace.push('start');
  if (path.endsWith('/leases/stratum-operator')) api.trace.push('end');
  const hit = api.routes[path];
  if (hit instanceof Response) return hit;
  if (hit === undefined) return { ok: false, status: 404, json: async () => null };
  if (typeof hit === 'number') return { ok: false, status: hit, json: async () => null };
  return { ok: true, status: 200, json: async () => hit };
};
console.warn = () => {}; console.log = () => {};

const { loadLiveK8S } = await import('../src/live.js');
const live = await import('../src/live.js');
const session = await import('../src/session.js');
const { K8S, resetK8S } = await import('../src/data.js');
const { getConfig } = await import('../src/cluster-context.js');

const list = (...items) => ({ items });
const SECRET_TABLE = '/api/v1/secrets?fieldSelector=type%3Dhelm.sh%2Frelease.v1&includeObject=Metadata';
const secretTable = (...secrets) => ({
  kind: 'Table', apiVersion: 'meta.k8s.io/v1',
  columnDefinitions: [{ name: 'Name' }, { name: 'Type' }, { name: 'Data' }, { name: 'Age' }],
  rows: secrets.map(({ metadata, type, keys }) => ({
    cells: [metadata.name, type, keys, '1d'],
    object: { kind: 'PartialObjectMetadata', apiVersion: 'meta.k8s.io/v1', metadata },
  })),
});
const node = (name, { ready = 'True', cordoned = false, cp = false, taints, conditions, stratum, kubelet = 'v1.34.1' } = {}) => ({
  metadata: {
    name, creationTimestamp: new Date().toISOString(),
    ...(stratum ? { annotations: { 'stratum.ciq.com/k8s-version': stratum } } : {}),
    labels: { 'kubernetes.io/arch': 'arm64', ...(cp ? { 'node-role.kubernetes.io/control-plane': '' } : {}) },
  },
  spec: { unschedulable: cordoned, ...(taints ? { taints } : {}) },
  status: {
    allocatable: { cpu: '4', memory: '8Gi', pods: '110' },
    nodeInfo: { kubeletVersion: kubelet },
    conditions: conditions || [{ type: 'Ready', status: ready }],
  },
});
const ingress = (name) => ({ metadata: { name, namespace: 'default' }, spec: {} });
const event = (name) => ({ metadata: { name, namespace: 'default' },
  type: 'Warning', reason: 'BackOff', message: 'back-off restarting failed container',
  involvedObject: { kind: 'Pod', name: 'x-1' }, count: 2, lastTimestamp: new Date().toISOString() });
const pod = (name, node, ns = 'kube-system') => ({ metadata: { name, namespace: ns, annotations: { 'kubernetes.io/config.mirror': 'x' } },
  spec: { nodeName: node, containers: [] }, status: { phase: 'Running' } });
const summary = (name) => ({ node: { cpu: { usageNanoCores: 1e9 }, memory: { workingSetBytes: 1073741824 } }, pods: [] });

// A cluster that answers everything, so each test only has to state its deltas.
function cluster(over = {}) {
  api.routes = {
    '/version': { gitVersion: 'v1.34.1' },
    '/api/v1/nodes': list(node('cp-1', { cp: true })),
    '/api/v1/pods': list(), '/api/v1/namespaces': list({ metadata: { name: 'default' }, status: { phase: 'Active' } }),
    '/api/v1/services': list(), '/apis/apps/v1/deployments': list(), '/apis/apps/v1/statefulsets': list(),
    '/apis/apps/v1/daemonsets': list(), '/apis/batch/v1/jobs': list(), '/apis/batch/v1/cronjobs': list(),
    '/apis/networking.k8s.io/v1/ingresses': list(ingress('web')),
    '/api/v1/persistentvolumeclaims': list(), '/api/v1/persistentvolumes': list(),
    '/apis/storage.k8s.io/v1/storageclasses': list(), '/api/v1/configmaps': list(), [SECRET_TABLE]: secretTable(),
    '/api/v1/serviceaccounts': list(), '/apis/rbac.authorization.k8s.io/v1/clusterroles': list(),
    '/apis/rbac.authorization.k8s.io/v1/roles': list(), '/apis/rbac.authorization.k8s.io/v1/clusterrolebindings': list(),
    '/apis/rbac.authorization.k8s.io/v1/rolebindings': list(), '/api/v1/events': list(),
    '/apis/coordination.k8s.io/v1/namespaces/kube-system/leases/stratum-operator': 404,
    ...over,
  };
}

test('a failed list keeps the last good data; a successful empty one replaces it', async () => {
  resetK8S();
  cluster({
    '/api/v1/nodes': list(node('cp-1', { cp: true }), node('w-1')),
    '/api/v1/events': list(event('e1')),
    '/api/v1/namespaces': list({ metadata: { name: 'default' }, status: { phase: 'Active' } }),
  });
  await loadLiveK8S();
  assert.equal(K8S.NODES.length, 2);
  assert.equal(K8S.EVENTS.length, 1);
  assert.deepEqual(K8S.NAMESPACES, ['all-namespaces', 'default']);

  // A 503 and a 403 are not "the cluster has none of these".
  cluster({ '/api/v1/nodes': 503, '/api/v1/events': 403, '/api/v1/namespaces': 503 });
  await loadLiveK8S();
  assert.equal(K8S.NODES.length, 2, 'a failed node list must not clear the nodes');
  assert.equal(K8S.EVENTS.length, 1, 'a failed event list must not clear the events');
  assert.deepEqual(K8S.NAMESPACES, ['all-namespaces', 'default'], 'a failed namespace list must not clear the filter');
  assert.equal(K8S.CLUSTER.nodeCount, 2, 'nor rebuild the cluster identity from nothing');

  // An answered, genuinely empty cluster does replace it.
  cluster({ '/api/v1/nodes': list(), '/api/v1/events': list(), '/api/v1/namespaces': list() });
  await loadLiveK8S();
  assert.equal(K8S.NODES.length, 0);
  assert.equal(K8S.EVENTS.length, 0);
  assert.deepEqual(K8S.NAMESPACES, ['all-namespaces']);
});

test('a malformed successful list preserves its last good rows and reports them stale', async () => {
  resetK8S();
  cluster({ '/api/v1/pods': list(pod('existing', 'cp-1')) });
  await loadLiveK8S();
  const previous = K8S.PODS;
  for (const body of ['<html>proxy error</html>', '', 'null', '{}', '{"message":"proxy error"}',
    '{"items":null}', '{"items":{}}', '[]', '"proxy error"']) {
    cluster({ '/api/v1/pods': new Response(body, { status: 200 }) });
    const outcome = await loadLiveK8S();
    assert.equal(K8S.PODS, previous, 'keep the last readable pod list');
    assert.ok(outcome.stale.includes('PODS'), 'retained pod data must be marked stale');
    assert.match(live.refreshVerdict(outcome).message, /kept last data/);
  }
});

test('malformed VersionInfo preserves cluster versions and marks their slices stale', async () => {
  resetK8S();
  cluster();
  await loadLiveK8S();
  const before = { cluster: structuredClone(K8S.CLUSTER), info: structuredClone(K8S.CLUSTER_INFO) };
  assert.equal(before.cluster.k8sVersion, '1.34.1');
  for (const version of [{}, [], 'v1.37.1', { message: 'proxy error' },
    { gitVersion: null }, { gitVersion: 137 }, { gitVersion: '' }, { gitVersion: '  ' }]) {
    cluster({ '/version': version });
    const outcome = await loadLiveK8S();
    assert.deepEqual(K8S.CLUSTER, before.cluster);
    assert.deepEqual(K8S.CLUSTER_INFO, before.info);
    assert.ok(outcome.stale.includes('CLUSTER') && outcome.stale.includes('CLUSTER_INFO'));
  }
  cluster({ '/version': { gitVersion: 'v1.37.1-stratum' } });
  const recovered = await loadLiveK8S();
  assert.equal(K8S.CLUSTER.k8sVersion, '1.37.1-stratum');
  assert.equal(K8S.CLUSTER_INFO.version, '1.37.1-stratum');
  assert.ok(!recovered.stale.includes('CLUSTER') && !recovered.stale.includes('CLUSTER_INFO'));
});

test('session expiry clears every cached slice and its live/version markers', async () => {
  resetK8S();
  const empty = structuredClone(K8S);
  cluster({ '/api/v1/pods': list(pod('private-a', 'cp-1')) });
  await loadLiveK8S();
  window.__STRATUM_K8S_VERSION = 'v1.34.1';
  assert.equal(K8S.PODS[0].name, 'private-a');
  session.clearSession();
  assert.deepEqual(K8S, empty, 'ended sessions must not retain any authorized rows or metrics');
  assert.equal(window.__STRATUM_LIVE, false);
  assert.equal(window.__STRATUM_K8S_VERSION, '');
});

test('delayed and queued loads cannot carry one user\'s snapshot into the next session', async (t) => {
  const baseFetch = globalThis.fetch;
  let actor = 'a', heldPath, started, release, hold = false;
  t.mock.method(globalThis, 'fetch', async (url, opts) => {
    const path = String(url).replace(/^\/k8s/, '');
    if (path === '/api/v1/login') return Response.json({ user: { username: actor }, csrfToken: actor });
    if (path === '/api/v1/logout') return new Response(null, { status: 204 });
    const answer = baseFetch(url, opts);
    if (hold && path === heldPath) {
      hold = false;
      started();
      await new Promise((resolve) => { release = resolve; });
    }
    return answer;
  });
  // Hold both before the first write and after the node/pod slices were
  // written, while the later posture reads are still waiting.
  for (heldPath of ['/api/v1/pods', '/apis/coordination.k8s.io/v1/namespaces/kube-system/leases/stratum-operator']) {
    actor = 'a';
    resetK8S();
    const empty = structuredClone(K8S);
    cluster({ '/api/v1/pods': list(pod('private-a', 'cp-1')) });
    await session.login('a-token');
    await loadLiveK8S();
    assert.equal(K8S.PODS[0].name, 'private-a');
    window.__STRATUM_K8S_VERSION = 'v1.34.1';
    const waiting = new Promise((resolve) => { started = resolve; });
    hold = true;
    const previous = loadLiveK8S();
    await waiting;
    const oldQueued = loadLiveK8S();
    let current;
    try {
      await session.logout();
      assert.deepEqual(K8S, empty, 'logout must clear A\'s last-good snapshot before B signs in');
      actor = 'b';
      await session.login('b-token');
      assert.equal(session.currentIdentity().username, 'b');
      for (const path of Object.keys(api.routes)) api.routes[path] = 403;
      current = loadLiveK8S();
      release();
      const [old, queued, latest] = await Promise.all([previous, oldQueued, current]);
      assert.equal(old.superseded, true, 'A\'s in-flight load must be discarded');
      assert.equal(queued.superseded, true, 'A\'s queued reload must also be discarded');
      assert.notEqual(latest.superseded, true, 'B must receive its own load, not the old queued outcome');
      assert.equal(latest.answered, false, 'B\'s forbidden reads answer no slices');
      assert.deepEqual(K8S, empty, 'retaining last-good data under B\'s 403 must not reveal A\'s rows');
      assert.equal(window.__STRATUM_LIVE, false, 'discarded loads must not restore the live marker');
      assert.equal(window.__STRATUM_K8S_VERSION, '');
      assert.deepEqual(live.refreshVerdict(old), { stamp: false, message: null });
    } finally {
      hold = false;
      release();
      await Promise.allSettled([previous, oldQueued, current]);
      session.clearSession();
      resetK8S();
    }
  }
});

test('a load started while login is pending cannot commit the previous cookie\'s rows', async (t) => {
  resetK8S();
  const empty = structuredClone(K8S);
  cluster({ '/api/v1/pods': list(pod('private-a', 'cp-1')) });
  let releaseLogin;
  t.mock.method(globalThis, 'fetch', async (url) => {
    assert.equal(String(url), '/api/v1/login', 'reads must wait until the cookie stops changing');
    return new Promise((resolve) => { releaseLogin = resolve; });
  });
  const signingIn = session.login('b-token');
  const oldCookieLoad = loadLiveK8S();
  try {
    assert.equal((await oldCookieLoad).superseded, true);
    releaseLogin(Response.json({ user: { username: 'b' }, csrfToken: 'b' }));
    await signingIn;
    assert.equal(session.currentIdentity().username, 'b');
    assert.deepEqual(K8S, empty, 'responses sent with A\'s cookie cannot populate B\'s cache');
    assert.equal(window.__STRATUM_LIVE, false);
  } finally {
    releaseLogin?.(Response.json({ user: { username: 'b' }, csrfToken: 'b' }));
    await Promise.allSettled([signingIn, oldCookieLoad]);
    session.clearSession();
  }
});

test('all loader list endpoints reject malformed successful envelopes', async () => {
  const paths = ['/api/v1/nodes', '/api/v1/pods', '/api/v1/namespaces',
    '/apis/apps/v1/deployments', '/apis/apps/v1/statefulsets', '/apis/apps/v1/daemonsets',
    '/apis/batch/v1/jobs', '/apis/batch/v1/cronjobs', '/apis/storage.k8s.io/v1/storageclasses',
    '/api/v1/events'];
  const deployment = { metadata: { name: 'web', namespace: 'default' }, spec: { replicas: 1 }, status: { readyReplicas: 1 } };
  const good = { '/api/v1/nodes/cp-1/proxy/stats/summary': summary('cp-1'),
    '/api/v1/pods': list(pod('web-1', 'cp-1', 'default')),
    '/apis/apps/v1/deployments': list(deployment), '/api/v1/events': list(event('warning')) };
  for (const path of paths) {
    resetK8S();
    cluster(good);
    await loadLiveK8S();
    const before = structuredClone(K8S);
    cluster({ ...good, [path]: { message: 'proxy error' } });
    const outcome = await loadLiveK8S();
    for (const slice of outcome.stale) assert.deepEqual(K8S[slice], before[slice], `${path} must preserve ${slice}`);
    assert.ok(outcome.stale.length > 0, `${path} must not be reported as a fresh list`);
  }
});

test('malformed Secret tables and server summaries preserve Helm metadata', async () => {
  const release = { metadata: { name: 'sh.helm.release.v1.web.v1', namespace: 'default',
    labels: { name: 'web', version: '1', status: 'deployed' } }, type: 'helm.sh/release.v1', keys: 1 };
  const config = (secretSummaries) => ({ mode: 'standalone', auth: { required: true }, features: { secretSummaries } });
  try {
    for (const served of [false, true]) {
      resetK8S();
      const path = served ? '/api/v1/secret-summaries' : SECRET_TABLE;
      const good = served ? list({ name: release.metadata.name, namespace: 'default',
        type: release.type, keys: release.keys, creationTimestamp: null,
        helm: { name: 'web', version: '1', status: 'deployed' } }) : secretTable(release);
      cluster({ '/api/v1/config': config(served), [path]: good });
      await getConfig({ force: true });
      await loadLiveK8S();
      const before = structuredClone(K8S.HELM);
      assert.equal(before.length, 1);
      const invalid = served
        ? [{}, { items: null }, { items: {} }]
        : [{}, { columnDefinitions: [], rows: {} }, { rows: [] },
          { ...secretTable(release), rows: [{ cells: null }] }];
      for (const body of invalid) {
        cluster({ [path]: body });
        const outcome = await loadLiveK8S();
        assert.deepEqual(K8S.HELM, before, 'malformed metadata must retain the release');
        assert.ok(outcome.stale.includes('HELM'));
      }
      cluster({ [path]: served ? list() : secretTable() });
      const empty = await loadLiveK8S();
      assert.deepEqual(K8S.HELM, [], 'a genuine empty representation still clears releases');
      assert.ok(!empty.stale.includes('HELM'));
    }
  } finally {
    cluster({ '/api/v1/config': config(false) });
    await getConfig({ force: true });
  }
});

test('cordoning a node does not make it NotReady', async () => {
  resetK8S();
  cluster({ '/api/v1/nodes': list(node('cp-1', { cp: true }), node('w-1', { cordoned: true })) });
  await loadLiveK8S();
  const [, w] = K8S.NODES;
  assert.equal(w.ready, true);
  assert.equal(w.cordoned, true);
  assert.equal(w.status, 'SchedulingDisabled', 'the pill still says Cordoned');
  assert.equal(K8S.CLUSTER.readyNodes, 2, 'a cordoned node is ready');
  assert.equal(K8S.CLUSTER.status, 'healthy');

  // NotReady outranks Cordoned, so nothing hides a broken node.
  cluster({ '/api/v1/nodes': list(node('w-1', { ready: 'False', cordoned: true })) });
  await loadLiveK8S();
  assert.equal(K8S.NODES[0].ready, false);
  assert.equal(K8S.NODES[0].status, 'NotReady');
  assert.equal(K8S.CLUSTER.status, 'degraded');
});

test('nodes carry their real conditions, labels and taints', async () => {
  resetK8S();
  cluster({ '/api/v1/nodes': list(node('w-1', {
    conditions: [{ type: 'Ready', status: 'True' }, { type: 'MemoryPressure', status: 'True' }],
    taints: [{ key: 'node-role.kubernetes.io/control-plane', effect: 'NoSchedule' }, { key: 'k', value: 'v', effect: 'NoExecute' }],
  })) });
  await loadLiveK8S();
  const n = K8S.NODES[0];
  assert.equal(n.conditions.MemoryPressure, 'True', 'pressure is read, never assumed False');
  assert.equal(n.conditions.DiskPressure, undefined, 'a condition the node does not report stays unknown');
  assert.equal(n.labels['kubernetes.io/arch'], 'arm64');
  assert.deepEqual(n.taints, ['node-role.kubernetes.io/control-plane:NoSchedule', 'k=v:NoExecute']);
});

test('a condition no node reports is not a condition that is False everywhere', async () => {
  resetK8S();
  cluster({ '/api/v1/nodes': list(
    node('a', { conditions: [{ type: 'Ready', status: 'True' }, { type: 'MemoryPressure', status: 'False' }] }),
    node('b', { conditions: [{ type: 'Ready', status: 'True' }, { type: 'MemoryPressure', status: 'Unknown' }] }),
  ) });
  await loadLiveK8S();
  const by = Object.fromEntries(K8S.NODE_CONDITIONS.map((c) => [c.type, c]));
  assert.deepEqual(by.MemoryPressure.trueNodes, []);
  assert.deepEqual(by.MemoryPressure.unknownNodes, ['b'], 'Unknown is not False');
  // Nobody reports DiskPressure at all, which the card must not read as healthy.
  assert.deepEqual(by.DiskPressure.unknownNodes, ['a', 'b']);
  assert.equal(by.DiskPressure.nodes, 2);
});

test('a derived slice is not replaced when the read it derives from failed', async () => {
  resetK8S();
  const fresh = { metadata: { name: 'stratum-operator' },
    spec: { holderIdentity: 'op-1', renewTime: new Date().toISOString(), leaseDurationSeconds: 15 } };
  const full = {
    // the annotation is a version behind the kubelet, i.e. drifted
    '/api/v1/nodes': list(node('cp-1', { cp: true, stratum: 'v1.33.0' })),
    '/api/v1/pods': list(pod('etcd-cp-1', 'cp-1')),
    '/apis/apps/v1/daemonsets': list({ metadata: { name: 'cilium', namespace: 'kube-system' },
      spec: { template: { spec: { containers: [{ image: 'cilium:v1' }] } } }, status: { desiredNumberScheduled: 1, numberReady: 1 } }),
    '/apis/coordination.k8s.io/v1/namespaces/kube-system/leases/stratum-operator': fresh,
  };
  cluster(full);
  await loadLiveK8S();
  assert.equal(K8S.NODES[0].pods, 1);
  assert.equal(K8S.CLUSTER_INFO.cni, 'Cilium');

  // Node rows count pods and, without the Summary API, price them from requests.
  cluster({ ...full, '/api/v1/pods': 403 });
  await loadLiveK8S();
  assert.equal(K8S.NODES[0].pods, 1, 'a node list alone must not rewrite the rows with zero pods');

  // CLUSTER_INFO counts control-plane nodes: it is a dependant of the node list too.
  cluster({ ...full, '/api/v1/nodes': 503 });
  await loadLiveK8S();
  assert.equal(K8S.CLUSTER_INFO.controlPlane, 1, 'a failed node list must not zero the control-plane count');

  // The Overview cards depend on six reads; one failure must not blank a true card.
  cluster({ ...full, '/apis/apps/v1/daemonsets': 503 });
  await loadLiveK8S();
  assert.equal(K8S.CLUSTER_INFO.cni, 'Cilium');

  // A 404 on the lease IS an answer — there is no operator — and does update.
  cluster({ ...full, '/apis/coordination.k8s.io/v1/namespaces/kube-system/leases/stratum-operator': 404 });
  await loadLiveK8S();
  assert.equal(K8S.CLUSTER_INFO.operatorActive, false);
  assert.equal(K8S.CLUSTER_INFO.operatorLeader, '—');

  // A 500 on it is not: that would report the operator gone on a bad read.
  cluster({ ...full, '/apis/coordination.k8s.io/v1/namespaces/kube-system/leases/stratum-operator': fresh });
  await loadLiveK8S();
  cluster({ ...full, '/apis/coordination.k8s.io/v1/namespaces/kube-system/leases/stratum-operator': 500 });
  await loadLiveK8S();
  assert.equal(K8S.CLUSTER_INFO.operatorActive, true, 'a failed lease read keeps the last good operator state');
});

test('a dead operator is not Active', async () => {
  resetK8S();
  const fresh = { metadata: { name: 'stratum-operator' },
    spec: { holderIdentity: 'op-1', renewTime: new Date().toISOString(), leaseDurationSeconds: 15 } };
  cluster({ '/apis/coordination.k8s.io/v1/namespaces/kube-system/leases/stratum-operator': fresh });
  await loadLiveK8S();
  assert.equal(K8S.CLUSTER_INFO.operatorActive, true);

  // The same lease, last renewed an hour ago: the holder is gone.
  const stale = { ...fresh, spec: { ...fresh.spec, renewTime: new Date(Date.now() - 3600e3).toISOString() } };
  cluster({ '/apis/coordination.k8s.io/v1/namespaces/kube-system/leases/stratum-operator': stale });
  await loadLiveK8S();
  assert.equal(K8S.CLUSTER_INFO.operatorActive, false);
  assert.equal(K8S.CLUSTER_INFO.operatorLeader, 'op-1', 'but the stale holder is still named');
});

test('loads never overlap, and one issued during a load sees what comes after it', async () => {
  resetK8S();
  cluster({ '/api/v1/nodes': list(node('cp-1', { cp: true })) });
  api.trace = [];
  const first = loadLiveK8S();
  // A Refresh click (or the cordon handler's reload) landing mid-load. It must
  // not race the load already running into the same K8S singleton.
  cluster({ '/api/v1/nodes': list(node('cp-1', { cp: true }), node('w-1')) });
  const second = loadLiveK8S();
  await Promise.all([first, second]);

  assert.deepEqual(api.trace, ['start', 'end', 'start', 'end'], 'two loads, back to back, never interleaved');
  assert.equal(K8S.NODES.length, 2, 'the queued load ran after the first, so the newer answer wins');
});

test('a cluster that answered with no nodes has no fleet totals left over', async () => {
  resetK8S();
  cluster({
    '/api/v1/nodes': list(node('cp-1', { cp: true })),
    '/api/v1/nodes/cp-1/proxy/stats/summary': summary('cp-1'),
  });
  await loadLiveK8S();
  assert.equal(K8S.FLEET.nodes, 1);
  assert.equal(K8S.FLEET.cpuPct, 25);           // 1 core used of 4 allocatable
  assert.equal(K8S.FLEET.history.cpu.length, 1);

  cluster({ '/api/v1/nodes': list() });
  await loadLiveK8S();
  assert.equal(K8S.FLEET.nodes, 0, 'the totals go when the nodes do');
  assert.equal(K8S.FLEET.cpuPct, null);
  assert.deepEqual(K8S.FLEET.history, { cpu: [], mem: [] });

  // A node list that FAILED is not an empty cluster, so the last good fleet stays.
  cluster({
    '/api/v1/nodes': list(node('cp-1', { cp: true })),
    '/api/v1/nodes/cp-1/proxy/stats/summary': summary('cp-1'),
  });
  await loadLiveK8S();
  cluster({ '/api/v1/nodes': 503 });
  await loadLiveK8S();
  assert.equal(K8S.FLEET.nodes, 1);
});

test('failed kubelet summaries keep fleet data and mark it stale', async () => {
  resetK8S();
  cluster({ '/api/v1/nodes': list(node('cp-1', { cp: true })),
    '/api/v1/nodes/cp-1/proxy/stats/summary': summary('cp-1') });
  await loadLiveK8S();
  const before = structuredClone(K8S.FLEET);
  cluster({ '/api/v1/nodes': list(node('cp-1', { cp: true })),
    '/api/v1/nodes/cp-1/proxy/stats/summary': 503 });
  const outcome = await loadLiveK8S();
  assert.deepEqual(K8S.FLEET, before, 'unavailable usage must keep the last sample');
  assert.ok(outcome.stale.includes('FLEET'), 'the retained sample must be reported as stale');
  cluster({ '/api/v1/nodes': list(node('cp-1', { cp: true })),
    '/api/v1/nodes/cp-1/proxy/stats/summary': 401 });
  const expired = await loadLiveK8S();
  assert.equal(expired.unauthorized, true, 'a Summary 401 also ends the session');
  assert.ok(expired.stale.includes('FLEET'));
});

test('a partial kubelet summary refresh preserves the whole fleet sample and history', async () => {
  resetK8S();
  const nodes = list(node('a'), node('b'));
  cluster({ '/api/v1/nodes': nodes,
    '/api/v1/nodes/a/proxy/stats/summary': summary('a'),
    '/api/v1/nodes/b/proxy/stats/summary': summary('b') });
  await loadLiveK8S();
  const before = structuredClone(K8S.FLEET);
  assert.equal(before.nodes, 2);

  cluster({ '/api/v1/nodes': nodes,
    '/api/v1/nodes/a/proxy/stats/summary': summary('a'),
    '/api/v1/nodes/b/proxy/stats/summary': 503 });
  const outcome = await loadLiveK8S();
  assert.deepEqual(K8S.FLEET, before, 'a subset of nodes must not replace the cluster sample or append history');
  assert.ok(outcome.stale.includes('FLEET'));
  assert.equal(K8S.NODES.length, 2, 'node rows can still refresh with request-based fallback');

  cluster({ '/api/v1/nodes': nodes,
    '/api/v1/nodes/a/proxy/stats/summary': summary('a'),
    '/api/v1/nodes/b/proxy/stats/summary': summary('b') });
  const recovered = await loadLiveK8S();
  assert.ok(!recovered.stale.includes('FLEET'));
  assert.equal(K8S.FLEET.history.cpu.length, before.history.cpu.length + 1);
});

test('incomplete kubelet summaries retain workload usage while readiness refreshes', async () => {
  const deployment = (ready) => ({ metadata: { name: 'web', namespace: 'default' },
    spec: { replicas: 2 }, status: { readyReplicas: ready, availableReplicas: ready } });
  const usage = (name, cpu, mem) => ({ ...summary(name), pods: [{ podRef: { namespace: 'default', name },
    cpu: { usageNanoCores: cpu * 1e6 }, memory: { workingSetBytes: mem * 1048576 } }] });
  const good = { '/api/v1/nodes': list(node('a'), node('b')),
    '/apis/apps/v1/deployments': list(deployment(2)),
    '/api/v1/nodes/a/proxy/stats/summary': usage('web-a', 100, 10),
    '/api/v1/nodes/b/proxy/stats/summary': usage('web-b', 200, 20) };
  resetK8S();
  cluster(good);
  await loadLiveK8S();
  assert.equal(K8S.WORKLOADS[0].cpu, 300);
  assert.equal(K8S.WORKLOADS[0].mem, 30);
  for (const bad of [503, {}, { node: {}, pods: {} }]) {
    cluster({ ...good, '/apis/apps/v1/deployments': list(deployment(1)),
      '/api/v1/nodes/a/proxy/stats/summary': usage('web-a', 900, 90),
      '/api/v1/nodes/b/proxy/stats/summary': bad });
    const outcome = await loadLiveK8S();
    assert.equal(K8S.WORKLOADS[0].cpu, 300, 'a partial sample must not replace complete workload usage');
    assert.equal(K8S.WORKLOADS[0].mem, 30);
    assert.equal(K8S.WORKLOADS[0].ready, '1/2', 'current readiness remains independent of stale metrics');
    assert.ok(outcome.stale.includes('WORKLOADS'));
  }
  cluster({ ...good, '/api/v1/nodes/b/proxy/stats/summary': usage('web-b', 400, 40) });
  const recovered = await loadLiveK8S();
  assert.ok(!recovered.stale.includes('WORKLOADS'));
  assert.equal(K8S.WORKLOADS[0].ready, '2/2');
  assert.equal(K8S.WORKLOADS[0].cpu, 500, 'complete recovery must replace stale usage with the new sample');
  assert.equal(K8S.WORKLOADS[0].mem, 50);
});

test('missing pod metrics retain workload usage without discarding valid node measurements', async () => {
  resetK8S();
  const ref = { namespace: 'default', name: 'web-a' };
  const podStats = { podRef: ref, cpu: { usageNanoCores: 300e6 }, memory: { workingSetBytes: 30 * 1048576 } };
  const good = { '/api/v1/nodes': list(node('a')),
    '/apis/apps/v1/deployments': list({ metadata: { name: 'web', namespace: 'default' }, spec: { replicas: 1 } }),
    '/api/v1/nodes/a/proxy/stats/summary': { ...summary('a'), pods: [podStats] } };
  cluster(good);
  await loadLiveK8S();
  assert.equal(K8S.WORKLOADS[0].cpu, 300);
  assert.equal(K8S.WORKLOADS[0].mem, 30);
  for (const stats of [{ podRef: ref }, { podRef: ref, cpu: null, memory: null },
    { ...podStats, cpu: { usageNanoCores: '300000000' } }, { ...podStats, memory: {} }]) {
    cluster({ ...good, '/api/v1/nodes/a/proxy/stats/summary': {
      node: { cpu: { usageNanoCores: 2e9 }, memory: { workingSetBytes: 2 * 1073741824 } }, pods: [stats],
    } });
    const outcome = await loadLiveK8S();
    assert.equal(K8S.WORKLOADS[0].cpu, 300, 'unreported pod usage is not zero');
    assert.equal(K8S.WORKLOADS[0].mem, 30);
    assert.ok(outcome.stale.includes('WORKLOADS'));
    assert.equal(K8S.FLEET.cpuUsed, 2, 'valid node usage still refreshes independently');
    assert.ok(!outcome.stale.includes('FLEET'));
  }
  cluster({ ...good, '/api/v1/nodes/a/proxy/stats/summary': { ...summary('a'), pods: [{
    podRef: ref, cpu: { usageNanoCores: 0 }, memory: { workingSetBytes: 0 },
  }] } });
  const zero = await loadLiveK8S();
  assert.equal(K8S.WORKLOADS[0].cpu, 0, 'an explicitly measured zero is a complete new sample');
  assert.equal(K8S.WORKLOADS[0].mem, 0);
  assert.ok(!zero.stale.includes('WORKLOADS'));
});

test('every request carries a deadline, and the Overview claims no posture verdict', async () => {
  resetK8S();
  api.unbounded = 0;
  cluster({ '/api/v1/nodes': list(node('cp-1', { cp: true })) });
  await loadLiveK8S();
  assert.equal(api.unbounded, 0, 'a request without an abort signal can hang the poll forever');

  // No posture verdict is derived from a namespace carrying the restricted label.
  assert.equal(K8S.CLUSTER_INFO.hardened, undefined);
  assert.equal(K8S.CLUSTER_INFO.signals, undefined);
});

// The header count read every healthy node as drifted, because the
// annotation holds the upstream version and the kubelet holds the SKE build of
// it — the `-stratum` suffix tools/inttest/check-single-node.sh asserts. An
// always-on indicator hides the partial upgrade it exists to catch. The
// loader's drift-summary slice is gone (no screen reads it), so what the loader
// must still provide is the version pair on each row, read by nodeDrift() —
// the same comparison the Nodes screen's row badge and header count run.
test('the SKE build suffix is not version drift, a different upstream version is', async () => {
  resetK8S();
  cluster({ '/api/v1/nodes': list(
    node('aligned', { cp: true, stratum: 'v1.37.0', kubelet: 'v1.37.0-stratum' }),
    node('skewed', { stratum: 'v1.37.0', kubelet: 'v1.36.4-stratum' }),
  ) });
  await loadLiveK8S();
  const rows = Object.fromEntries(K8S.NODES.map((n) => [n.name, n]));
  assert.deepEqual([rows.aligned.version, rows.aligned.stratumVersion],
    ['v1.37.0-stratum', 'v1.37.0'], 'the row carries kubelet and stamped versions verbatim');
  assert.deepEqual([rows.skewed.version, rows.skewed.stratumVersion],
    ['v1.36.4-stratum', 'v1.37.0']);
  assert.deepEqual(K8S.NODES.filter(nodeDrift).map((n) => n.name), ['skewed'],
    'the screen\'s drift comparison sees only the node whose upstream version differs');
});

// An ended session answers 401 on every read. A loader that came to treat a
// 401 as "the cluster has none of these" — an answered empty list, a result
// object, or a rejection after some slices were replaced — would blank the
// screens the moment the session ends, so the whole snapshot must deep-equal
// what the last good load left. The whole-object comparison holds both before
// and after any slice is removed from the loader.
test('a 401 on every read leaves the whole snapshot as it was', async () => {
  resetK8S();
  cluster({
    '/api/v1/nodes': list(node('cp-1', { cp: true })),
    '/api/v1/pods': list(pod('etcd-cp-1', 'cp-1')),
    '/apis/apps/v1/deployments': list({ metadata: { name: 'web', namespace: 'default' },
      spec: { replicas: 1, template: { spec: { containers: [{ image: 'web:1' }] } } },
      status: { availableReplicas: 1 } }),
    '/api/v1/events': list({ metadata: { name: 'e', namespace: 'default' },
      type: 'Warning', reason: 'BackOff', message: 'back-off restarting failed container',
      involvedObject: { kind: 'Pod', name: 'etcd-cp-1' } }),
  });
  await loadLiveK8S();
  const before = structuredClone(K8S);
  assert.ok(before.NODES.length && before.PODS.length && before.WORKLOADS.length && before.EVENTS.length,
    'the fixture must fill the snapshot before the 401s');

  for (const key of Object.keys(api.routes)) api.routes[key] = 401;
  await loadLiveK8S();   // must resolve, not reject
  assert.deepEqual(K8S, before, 'a 401 on every read must leave every slice at its last good value');
});

// The hand-written screens read these slices (app.jsx, overview.jsx, infra.jsx,
// workloads.jsx, terminal.jsx).
// The Helm list is the reason the Secret read must survive any pruning of the
// loader's reads: nobody else fills it, and a Secret read deleted with the
// unread ones would empty the Helm screen with no test failing.
test('every slice a screen reads is filled, and keeps its last good value', async () => {
  resetK8S();
  const extra = {
    '/api/v1/nodes': list(node('cp-1', { cp: true })),
    '/api/v1/nodes/cp-1/proxy/stats/summary': summary('cp-1'),
    '/api/v1/pods': list(pod('etcd-cp-1', 'cp-1')),
    '/apis/apps/v1/deployments': list({ metadata: { name: 'web', namespace: 'default' },
      spec: { replicas: 1, template: { spec: { containers: [{ image: 'web:1' }] } } },
      status: { availableReplicas: 1 } }),
    [SECRET_TABLE]: secretTable({
      metadata: { name: 'sh.helm.release.v1.web.v1', namespace: 'default',
        labels: { name: 'web', version: '3', status: 'deployed', owner: 'helm' } },
      type: 'helm.sh/release.v1', keys: 1,
    }),
    '/api/v1/events': list({ metadata: { name: 'e', namespace: 'default' },
      type: 'Warning', reason: 'BackOff', message: 'back-off restarting failed container',
      involvedObject: { kind: 'Pod', name: 'web-1', namespace: 'default' }, count: 3 }),
    '/apis/storage.k8s.io/v1/storageclasses': list({
      metadata: { name: 'local-path', annotations: { 'storageclass.kubernetes.io/is-default-class': 'true' } },
      provisioner: 'rancher.io/local-path', reclaimPolicy: 'Delete', volumeBindingMode: 'WaitForFirstConsumer',
    }),
  };
  cluster(extra);
  await loadLiveK8S();

  assert.equal(K8S.HELM.length, 1, 'the Helm screen reads HELM, filled from the release Secret');
  assert.equal(K8S.HELM[0].name, 'web');
  assert.equal(K8S.HELM[0].revision, 3);
  assert.equal(K8S.HELM[0].status, 'deployed');
  assert.equal(K8S.EVENTS.length, 1, 'the Overview reads EVENTS');
  assert.equal(K8S.EVENTS[0].type, 'Warning');
  assert.equal(K8S.STORAGE_CLASSES.length, 1, 'the Storage screen reads STORAGE_CLASSES');
  assert.equal(K8S.STORAGE_CLASSES[0].default, true, 'the default class stays marked as default');
  assert.equal(K8S.NODES.length, 1);
  assert.equal(K8S.PODS.length, 1);
  assert.equal(K8S.WORKLOADS.length, 1);
  assert.deepEqual(K8S.NAMESPACES, ['all-namespaces', 'default']);
  assert.equal(K8S.NODE_CONDITIONS.length, 4);
  assert.equal(K8S.FLEET.nodes, 1, 'the Overview cards read FLEET');
  assert.ok(K8S.CLUSTER_INFO, 'the Overview cards read CLUSTER_INFO');
  assert.ok(K8S.CLUSTER, 'the Shell header reads CLUSTER');

  // A 403 on the Secret and Event lists is not "the cluster has none": the
  // last good release and event must survive the failed load exactly.
  const helmWas = structuredClone(K8S.HELM);
  const eventsWas = structuredClone(K8S.EVENTS);
  cluster({ ...extra, [SECRET_TABLE]: 403, '/api/v1/events': 403 });
  await loadLiveK8S();
  assert.deepEqual(K8S.HELM, helmWas, 'a failed Secret list must not empty the Helm screen');
  assert.deepEqual(K8S.EVENTS, eventsWas, 'a failed Event list must not empty the events');
});

// No screen (app.jsx, overview.jsx, infra.jsx, workloads.jsx,
// terminal.jsx, shell.jsx) reads these slices. A slice nothing reads
// must not cost the shell a key, the loader a read, a rewrite and
// a stale entry every poll.
const REMOVED_SLICES = ['HARDENING', 'SERVICES', 'INGRESSES', 'PVCS', 'PVS', 'CONFIGMAPS', 'SECRETS',
  'SERVICE_ACCOUNTS', 'ROLES', 'BINDINGS', 'NAMESPACE_ROWS', 'VERSION_DRIFT'];
test('the loader does not fill the slices no screen reads', async () => {
  resetK8S();
  for (const slice of REMOVED_SLICES) {
    assert.ok(!(slice in K8S), `${slice} is gone from the empty shell`);
  }
  cluster();
  await loadLiveK8S();
  for (const slice of REMOVED_SLICES) {
    assert.ok(!(slice in K8S), `${slice} must not appear on a full load`);
  }
});

// The same audit, on the wire: one load against a cluster that answers
// everything, counted by path. The eleven list reads behind the removed slices
// are gone; the reads the screens' slices are built from stay, one call each — the
// Secret read among them, still feeding HELM.
const REMOVED_READS = ['/api/v1/services', '/apis/networking.k8s.io/v1/ingresses',
  '/api/v1/persistentvolumeclaims', '/api/v1/persistentvolumes', '/api/v1/configmaps',
  '/api/v1/serviceaccounts', '/apis/rbac.authorization.k8s.io/v1/clusterroles',
  '/apis/rbac.authorization.k8s.io/v1/roles',
  '/apis/rbac.authorization.k8s.io/v1/clusterrolebindings',
  '/apis/rbac.authorization.k8s.io/v1/rolebindings',
  '/apis/certificates.k8s.io/v1/certificatesigningrequests'];
test('a poll issues exactly the reads the screens\' slices are built from', async () => {
  resetK8S();
  cluster();
  api.calls = [];
  await loadLiveK8S();
  const issued = api.calls.map((c) => c.url.replace(/^\/k8s/, ''));
  for (const path of REMOVED_READS) {
    assert.ok(!issued.includes(path), `a poll must not read ${path} — no screen displays that list`);
  }
  const perPoll = {};
  for (const path of issued) perPoll[path] = (perPoll[path] || 0) + 1;
  assert.deepEqual(perPoll, {
    '/version': 1,
    '/api/v1/nodes': 1,
    '/api/v1/nodes/cp-1/proxy/stats/summary': 1,   // the kubelet Summary read, one per node
    '/api/v1/pods': 1,
    '/api/v1/namespaces': 1,
    '/apis/apps/v1/deployments': 1,
    '/apis/apps/v1/statefulsets': 1,
    '/apis/apps/v1/daemonsets': 1,
    '/apis/batch/v1/jobs': 1,
    '/apis/batch/v1/cronjobs': 1,
    '/api/v1/events': 1,
    '/apis/storage.k8s.io/v1/storageclasses': 1,
    '/apis/coordination.k8s.io/v1/namespaces/kube-system/leases/stratum-operator': 1,
    [SECRET_TABLE]: 1,                             // metadata-only release Secrets, feeds HELM
  }, 'every read a poll issues is accounted for, one call each');
});

// The live load runs every 15 s for every viewer, whatever screen is open, so
// it never asks for Secret objects: the Stratum server's own summary when the
// config says it serves one, otherwise a metadata-only Table of the Helm
// release Secrets alone through the proxy.
test('the live load never lists Secret objects', async () => {
  const secretReads = () => api.calls.filter((c) => /secret/.test(c.url));

  resetK8S();
  cluster({ [SECRET_TABLE]: secretTable(
    { metadata: { name: 'sh.helm.release.v1.api.v4', namespace: 'app', creationTimestamp: new Date().toISOString(),
      labels: { name: 'api', version: '4', status: 'deployed', owner: 'helm' } }, type: 'helm.sh/release.v1', keys: 1 },
  ) });
  api.calls = [];
  await loadLiveK8S();
  assert.deepEqual(secretReads(), [{ url: '/k8s' + SECRET_TABLE, accept: 'application/json;as=Table;g=meta.k8s.io;v=v1' }],
    'without the summary, only Helm release Secrets are read, as metadata');
  assert.deepEqual(K8S.HELM.map(({ name, ns, revision, status }) => ({ name, ns, revision, status })),
    [{ name: 'api', ns: 'app', revision: 4, status: 'deployed' }]);

  resetK8S();
  cluster({
    '/api/v1/config': { mode: 'standalone', k8sBase: '/k8s', auth: { required: true }, features: { secretSummaries: true } },
    '/api/v1/secret-summaries': list(
      { name: 'db', namespace: 'app', type: 'Opaque', keys: 2, creationTimestamp: new Date().toISOString() },
      { name: 'tls', namespace: 'app', type: 'kubernetes.io/tls', keys: 3, creationTimestamp: null },
      { name: 'sh.helm.release.v1.web.v2', namespace: 'app', type: 'helm.sh/release.v1', keys: 1, creationTimestamp: null,
        helm: { name: 'web', version: '2', status: 'deployed' } },
    ),
  });
  await getConfig({ force: true });
  api.calls = [];
  await loadLiveK8S();
  assert.deepEqual(secretReads().map((c) => c.url), ['/api/v1/secret-summaries'],
    'a server that serves the summary is asked for it, outside the proxy');
  assert.deepEqual(K8S.HELM.map(({ name, ns, revision, status }) => ({ name, ns, revision, status })),
    [{ name: 'web', ns: 'app', revision: 2, status: 'deployed' }]);

  delete api.routes['/api/v1/config'];
  await getConfig({ force: true });
});

// Resolving silently whatever happened would let the poll tick and the
// Refresh action stamp "refreshed" over data that never answered, and a
// 401 reach nobody. `load` resolves an outcome: `answered` says whether
// any slice of K8S was rewritten this round, `unauthorized` says the session
// ended (and the transport has routed it to sign-in through session.js's
// handler), and `stale` names — by their K8S slice keys — the slices that kept
// their last good data because a read failed. Staleness is decided on the
// cluster's own list reads, plus a retained FLEET sample when kubelet Summary
// usage is unavailable. Node rows may still fall back to request-based numbers.
test('a load reports whether the cluster answered, whether the session ended, and which slices went stale', async () => {
  const seen = [];
  session.setUnauthorizedHandler((path) => seen.push(path));
  try {
    resetK8S();
    cluster({ '/api/v1/nodes': list(node('cp-1', { cp: true })),
      '/api/v1/nodes/cp-1/proxy/stats/summary': summary('cp-1') });
    // A Refresh click queued behind a running load waits for the next one; it
    // must receive the outcome of that next load, not an empty resolution.
    const first = loadLiveK8S();
    const second = loadLiveK8S();
    const [o1, o2] = await Promise.all([first, second]);
    assert.deepEqual(o1, { answered: true, unauthorized: false, stale: [] });
    assert.deepEqual(o2, { answered: true, unauthorized: false, stale: [] },
      'a caller queued behind a running load also receives an outcome');

    // An ended session answers 401 on every read: nothing answered, every
    // slice kept its last good value, and the user is sent to sign-in through
    // the unauthorized handler session.js holds.
    resetK8S();
    cluster({ '/api/v1/nodes': list(node('cp-1', { cp: true })) });
    for (const key of Object.keys(api.routes)) api.routes[key] = 401;
    seen.length = 0;
    const gone = await loadLiveK8S();   // must resolve, not reject
    assert.equal(gone.answered, false);
    assert.equal(gone.unauthorized, true);
    for (const slice of ['NODES', 'PODS', 'EVENTS']) {
      assert.ok(gone.stale.includes(slice), `a 401 on every read must report ${slice} as stale`);
    }
    assert.ok(seen.length > 0, 'a load of nothing but 401s must reach the unauthorized handler');

    // A cluster that cannot be reached at all is a failed refresh, not an
    // ended session: nobody is sent to sign-in for a 503.
    for (const key of Object.keys(api.routes)) api.routes[key] = 503;
    seen.length = 0;
    const down = await loadLiveK8S();
    assert.equal(down.answered, false);
    assert.equal(down.unauthorized, false);
    assert.ok(down.stale.includes('NODES'), 'a total failure must report the frozen slices');
    assert.deepEqual(seen, [], 'a 503 must not send the user to sign-in');
  } finally { session.setUnauthorizedHandler(null); }
});

// One list failing must not just quietly keep the slices it gates: the load's
// outcome has to say which slices are now showing last-good data, by the same
// K8S slice keys the screens read. The node list gates the node rows and every
// slice derived from them — the fleet totals, the aggregated conditions, the
// cluster identity and the Overview cards.
test('a partial load reports exactly the slices that kept their last good data', async () => {
  resetK8S();
  cluster({ '/api/v1/nodes': list(node('cp-1', { cp: true }), node('w-1')) });
  await loadLiveK8S();
  const goodNodes = structuredClone(K8S.NODES);
  assert.equal(goodNodes.length, 2);

  cluster({ '/api/v1/nodes': 503 });
  const o = await loadLiveK8S();
  assert.equal(o.answered, true, 'everything but the node list answered, so the load did answer');
  assert.equal(o.unauthorized, false);
  assert.deepEqual([...o.stale].sort(),
    ['CLUSTER', 'CLUSTER_INFO', 'FLEET', 'NODES', 'NODE_CONDITIONS']);
  assert.deepEqual(K8S.NODES, goodNodes, 'and the stale slices keep their last good value');

  // A slice whose read answered is never reported stale.
  assert.ok(!o.stale.includes('PODS') && !o.stale.includes('EVENTS') && !o.stale.includes('WORKLOADS'));
});

// The refresh decision — stamp the "refreshed" time, toast, stay
// silent — is a plain function beside the loader, so the suite can read what
// "refreshed" means. Only data that actually answered may acquire a fresh
// timestamp: a total failure stamps nothing, an ended session stamps nothing
// (sign-in is the report), and a partial success stamps while naming the stale
// slices instead of claiming a full refresh.
test('the refresh verdict stamps only a load that answered, and reports failure and stale slices', async () => {
  const verdict = (outcome) => live.refreshVerdict(outcome);

  const full = verdict({ answered: true, unauthorized: false, stale: [] });
  assert.equal(full.stamp, true, 'a fully answered load refreshes the timestamp');
  assert.equal(full.message, 'Refreshed');

  const partial = verdict({ answered: true, unauthorized: false, stale: ['NODES', 'FLEET'] });
  assert.equal(partial.stamp, true, 'the slices that answered still count as refreshed');
  assert.notEqual(partial.message, 'Refreshed', 'a partial load must not claim every slice is current');
  assert.match(partial.message, /NODES/);
  assert.match(partial.message, /FLEET/);

  const failed = verdict({ answered: false, unauthorized: false, stale: ['NODES', 'PODS'] });
  assert.equal(failed.stamp, false, 'a total failure must not stamp success');
  assert.equal(failed.message, 'Refresh failed — keeping last data');

  const ended = verdict({ answered: false, unauthorized: true, stale: ['NODES'] });
  assert.equal(ended.stamp, false, 'an ended session must not stamp success');
  assert.equal(ended.message, null, 'sign-in is the report; no toast beside it');
});

// Stamping "refreshed Xs ago" whenever the poll tick settles would let an
// hour of failed polls — or an ended session — present frozen data as
// freshly refreshed. The shell also receives the last load's stale
// slices, and the label may only claim a refresh when none are.
test('the live toggle presents stale data as stale, never as freshly refreshed', async () => {
  const h = await harness();
  const AppShell = await h.component('ui/shell.jsx', 'AppShell');
  const render = (props) => AppShell({ active: 'overview', onNavigate: () => {}, counts: {}, crumbs: ['Overview'],
    namespace: 'all-namespaces', onNamespace: () => {}, namespaces: ['all-namespaces'],
    live: true, updated: Date.now() - 5000, onToggleLive: () => {}, onPalette: () => {},
    config: { links: {} }, cluster: { name: 'kube' }, me: null, onSignOut: null, children: null, ...props });
  const toggle = (tree) => collect(tree, (n) => n.type === 'button' && typeof n.props['aria-label'] === 'string'
    && /Live|Paused|stale/.test(n.props['aria-label']))[0];

  const fresh = toggle(render({ stale: [] })).props['aria-label'];
  assert.equal(fresh, 'Live — refreshed 5s ago; click to pause', 'a fully answered load keeps today\'s honest label');

  for (const stale of [['NODES', 'FLEET'], ['NODES', 'PODS', 'EVENTS']]) {
    const label = toggle(render({ stale })).props['aria-label'];
    assert.match(label, /stale/i, 'the label must say the data is stale');
    assert.doesNotMatch(label, /refreshed/i, 'the label must not present stale data as freshly refreshed');
  }

  const paused = toggle(render({ live: false, stale: [] })).props['aria-label'];
  assert.equal(paused, 'Paused — click to resume auto-refresh');
});

test('malformed successful Lease responses preserve operator state and report dependent slices stale', async () => {
  resetK8S();
  const path = '/apis/coordination.k8s.io/v1/namespaces/kube-system/leases/stratum-operator';
  const lease = { metadata: { name: 'stratum-operator' }, spec: {
    holderIdentity: 'op-1', renewTime: new Date().toISOString(), leaseDurationSeconds: 60,
  } };
  cluster({ [path]: lease });
  await loadLiveK8S();
  const info = structuredClone(K8S.CLUSTER_INFO);
  assert.equal(info.operatorActive, true);
  for (const body of [{}, [], { metadata: null }, { metadata: [] }, { metadata: {}, spec: [] }]) {
    cluster({ [path]: body });
    const outcome = await loadLiveK8S();
    assert.deepEqual(K8S.CLUSTER_INFO, info);
    assert.ok(outcome.stale.includes('CLUSTER_INFO'));
  }
  cluster({ [path]: 404 });
  const absent = await loadLiveK8S();
  assert.equal(K8S.CLUSTER_INFO.operatorActive, false);
  assert.equal(absent.stale.includes('CLUSTER_INFO'), false, 'a real 404 is an authoritative absent operator');
});

test('malformed server secret summaries preserve Helm metadata until a valid summary answers', async () => {
  resetK8S();
  const config = { mode: 'standalone', k8sBase: '/k8s', auth: { required: true }, features: { secretSummaries: true } };
  const path = '/api/v1/secret-summaries';
  const release = { name: 'sh.helm.release.v1.web.v2', namespace: 'app', type: 'helm.sh/release.v1',
    keys: 1, creationTimestamp: null, helm: { name: 'web', version: '2', status: 'deployed' } };
  cluster({ '/api/v1/config': config, [path]: list(release) });
  await getConfig({ force: true });
  try {
    await loadLiveK8S();
    const good = structuredClone(K8S.HELM);
    assert.equal(good.length, 1);
    for (const item of [{}, null, [], { ...release, name: 12 }, { ...release, namespace: null },
      { ...release, type: null }, { ...release, keys: '1' }, { ...release, keys: -1 },
      { ...release, creationTimestamp: {} }, { ...release, helm: null }, { ...release, helm: {} },
      { ...release, helm: { name: 'web', version: 2, status: 'deployed' } },
      { ...release, helm: undefined }]) {
      cluster({ [path]: list(item) });
      const outcome = await loadLiveK8S();
      assert.deepEqual(K8S.HELM, good);
      assert.ok(outcome.stale.includes('HELM'));
    }
    cluster({ [path]: list({ name: 'db', namespace: 'app', type: 'Opaque', keys: 0, creationTimestamp: null }) });
    const noReleases = await loadLiveK8S();
    assert.deepEqual(K8S.HELM, []);
    assert.equal(noReleases.stale.includes('HELM'), false, 'valid non-Helm summaries may remove old releases');
    cluster({ [path]: list() });
    assert.equal((await loadLiveK8S()).stale.includes('HELM'), false);
  } finally {
    delete api.routes['/api/v1/config'];
    await getConfig({ force: true });
  }
});

test('malformed Summary pod references retain usage while workload readiness refreshes', async () => {
  resetK8S();
  const deployment = (ready) => ({ metadata: { name: 'web', namespace: 'default' },
    spec: { replicas: 2 }, status: { readyReplicas: ready, availableReplicas: ready } });
  const usage = (name, cpu, mem, ref = { namespace: 'default', name }) => ({ ...summary(name),
    pods: [{ podRef: ref, cpu: { usageNanoCores: cpu * 1e6 }, memory: { workingSetBytes: mem * 1048576 } }] });
  const good = { '/api/v1/nodes': list(node('a'), node('b')),
    '/apis/apps/v1/deployments': list(deployment(2)),
    '/api/v1/nodes/a/proxy/stats/summary': usage('web-a', 100, 10),
    '/api/v1/nodes/b/proxy/stats/summary': usage('web-b', 200, 20) };
  cluster(good);
  await loadLiveK8S();
  const fleet = structuredClone(K8S.FLEET);
  assert.equal(K8S.WORKLOADS[0].cpu, 300);
  assert.equal(K8S.WORKLOADS[0].mem, 30);
  for (const ref of [{}, { name: 'web-a' }, { namespace: 'default' },
    { namespace: '', name: 'web-a' }, { namespace: 'default', name: '' },
    { namespace: 12, name: 'web-a' }, { namespace: 'default', name: null }]) {
    cluster({ ...good, '/apis/apps/v1/deployments': list(deployment(1)),
      '/api/v1/nodes/a/proxy/stats/summary': usage('web-a', 900, 90, ref) });
    const outcome = await loadLiveK8S();
    assert.equal(K8S.WORKLOADS[0].cpu, 300, 'an unkeyed sample cannot replace last-good workload CPU');
    assert.equal(K8S.WORKLOADS[0].mem, 30, 'an unkeyed sample cannot replace last-good workload memory');
    assert.equal(K8S.WORKLOADS[0].ready, '1/2', 'readiness still reflects its successful resource read');
    assert.deepEqual(K8S.FLEET, fleet, 'a malformed node Summary cannot contribute a partial fleet sample');
    assert.ok(outcome.stale.includes('WORKLOADS'));
    assert.ok(outcome.stale.includes('FLEET'));
  }
  cluster({ ...good,
    '/api/v1/nodes/a/proxy/stats/summary': usage('web-a', 0, 0),
    '/api/v1/nodes/b/proxy/stats/summary': usage('web-b', 0, 0) });
  const recovered = await loadLiveK8S();
  assert.equal(K8S.WORKLOADS[0].cpu, 0, 'zero is a valid reading with a complete reference');
  assert.equal(K8S.WORKLOADS[0].mem, 0);
  assert.equal(recovered.stale.includes('WORKLOADS'), false);
  assert.equal(recovered.stale.includes('FLEET'), false);
});
