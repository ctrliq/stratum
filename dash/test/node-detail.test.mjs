// Node details must show what the Node object reports: the allocatable CPU
// core count (not the instance-type label — on nodes without that label the
// Capacity row read "— vCPU") and the node's status.addresses, which no
// screen could show because live.js never mapped them onto the row.
//
// Two layers, both driving the real code:
//   live.js  — the real loadLiveK8S mapping from raw Node objects, through
//              the same fetch-seam harness live.test.mjs uses;
//   screens/infra.jsx NodeDetail — rendered the way nodes.test.mjs renders
//              it (inert tree, reading the row components' props).
//
// Row field names the fix must use: `cores` (number | null, allocatable
// cores parsed with the existing cpuToM quantity parser, capacity fallback)
// and `addresses` ([{type, address}] in API order).
import assert from 'node:assert/strict';
import test from 'node:test';

import { harness, collect, text } from './harness.mjs';

// live.js talks to the apiserver through the global fetch and stamps window on
// success, so both have to exist before it is imported (same seam as
// live.test.mjs).
globalThis.window = {};
Object.defineProperty(globalThis, 'navigator', { value: {}, configurable: true });
const api = { routes: {}, calls: [] };
globalThis.fetch = async (url, opts) => {
  api.calls.push({ url: String(url) });
  const path = String(url).replace(/^\/k8s/, '');
  const hit = api.routes[path];
  if (hit === undefined) return { ok: false, status: 404, json: async () => null };
  return { ok: true, status: 200, json: async () => hit };
};
console.warn = () => {}; console.log = () => {};

const { loadLiveK8S } = await import('../src/live.js');
const { K8S, resetK8S } = await import('../src/data.js');

const list = (...items) => ({ items });
const SECRET_TABLE = '/api/v1/secrets?fieldSelector=type%3Dhelm.sh%2Frelease.v1&includeObject=Metadata';

// A raw Node object the apiserver could have answered with.
const node = ({ allocatable, capacity, addresses } = {}) => ({
  metadata: {
    name: 'cp-1', creationTimestamp: new Date().toISOString(),
    labels: { 'node.kubernetes.io/instance-type': 'm6i.large' },
  },
  spec: {},
  status: {
    ...(allocatable ? { allocatable } : {}),
    ...(capacity ? { capacity } : {}),
    ...(addresses ? { addresses } : {}),
    nodeInfo: { kubeletVersion: 'v1.34.1' },
    conditions: [{ type: 'Ready', status: 'True' }],
  },
});

// A cluster that answers everything, so each test only states its deltas.
function cluster(over = {}) {
  api.routes = {
    '/version': { gitVersion: 'v1.34.1' },
    '/api/v1/nodes': list(node()),
    '/api/v1/pods': list(), '/api/v1/namespaces': list({ metadata: { name: 'default' }, status: { phase: 'Active' } }),
    '/api/v1/services': list(), '/apis/apps/v1/deployments': list(), '/apis/apps/v1/statefulsets': list(),
    '/apis/apps/v1/daemonsets': list(), '/apis/batch/v1/jobs': list(), '/apis/batch/v1/cronjobs': list(),
    '/apis/networking.k8s.io/v1/ingresses': list(),
    '/api/v1/persistentvolumeclaims': list(), '/api/v1/persistentvolumes': list(),
    '/apis/storage.k8s.io/v1/storageclasses': list(), '/api/v1/configmaps': list(), [SECRET_TABLE]: { kind: 'Table', rows: [] },
    '/api/v1/serviceaccounts': list(), '/apis/rbac.authorization.k8s.io/v1/clusterroles': list(),
    '/apis/rbac.authorization.k8s.io/v1/roles': list(), '/apis/rbac.authorization.k8s.io/v1/clusterrolebindings': list(),
    '/apis/rbac.authorization.k8s.io/v1/rolebindings': list(), '/api/v1/events': list(),
    '/apis/coordination.k8s.io/v1/namespaces/kube-system/leases/stratum-operator': 404,
    ...over,
  };
}

const loadNodes = async (over) => {
  resetK8S();
  cluster(over);
  await loadLiveK8S();
  return K8S.NODES.find((n) => n.name === 'cp-1');
};

test('the node row carries the allocatable core count, parsed with the cluster-card quantity parser', async () => {
  // "4" is the fixture live.test.mjs already uses for the / 4 cores card.
  assert.equal((await loadNodes({ '/api/v1/nodes': list(node({ allocatable: { cpu: '4', memory: '8Gi', pods: '110' } })) })).cores, 4,
    'allocatable.cpu "4" -> 4 cores');
  assert.equal((await loadNodes({ '/api/v1/nodes': list(node({ allocatable: { cpu: '3500m', memory: '8Gi', pods: '110' } })) })).cores, 3.5,
    'allocatable.cpu "3500m" -> 3.5 cores');
});

test('the core count falls back to capacity.cpu when allocatable is missing, and is unknown without either', async () => {
  assert.equal((await loadNodes({ '/api/v1/nodes': list(node({ capacity: { cpu: '8', memory: '16Gi', pods: '110' } })) })).cores, 8,
    'no allocatable but capacity.cpu "8" -> 8 cores');
  const row = await loadNodes({ '/api/v1/nodes': list(node({ capacity: { memory: '16Gi', pods: '110' } })) });
  assert.equal(row.cores, null, 'no cpu quantity at all -> unknown (null)');
});

test('the node row carries status.addresses in API order', async () => {
  const addresses = [
    { type: 'InternalIP', address: '10.78.0.11' },
    { type: 'Hostname', address: 'ske-e2e-ui-cp1' },
  ];
  const row = await loadNodes({ '/api/v1/nodes': list(node({ addresses })) });
  assert.deepEqual(row.addresses, addresses, 'addresses are mapped through in the order the object listed them');
  const bare = await loadNodes();
  assert.deepEqual(bare.addresses, [], 'a node without addresses maps to no address rows');
});

// The screen reads the row live.js built. Same inert-render approach as
// nodes.test.mjs: call NodeDetail directly and read the row components' props.
const renderDetail = async (nodeOver = {}) => {
  const h = await harness({ config: { mode: 'standalone', auth: { required: true } }, signedIn: true });
  await h.resolveConfig(); await h.signIn();
  const NodeDetail = await h.component('screens/infra.jsx', 'NodeDetail');
  const nodeRow = {
    name: 'cp-1', role: 'control-plane', zone: 'us-east-1a', status: 'Ready', ready: true, cordoned: false,
    cpu: 6, mem: 40, pods: 12, podCap: 110, age: '1d', instance: 'm6i.large',
    conditions: { Ready: 'True' }, labels: {}, taints: [],
    ...nodeOver,
  };
  const tree = NodeDetail({ node: nodeRow, data: {}, onAction: () => {} });
  const resRows = collect(tree, (x) => typeof x.type === 'function' && x.type.name === 'ResRow');
  const cpuRow = resRows.find((r) => r.props.label === 'CPU');
  assert.ok(cpuRow, 'the Capacity CPU row still renders');
  const kvpRows = collect(tree, (x) => typeof x.type === 'function' && x.type.name === 'KvP')
    .map((x) => [x.props.k, x.props.v]);
  return { text: text(tree), cpuDetail: cpuRow.props.detail, kvpRows };
};

test('Capacity CPU shows the allocatable core count as "<n> vCPU", not the instance-type label', async () => {
  const { cpuDetail } = await renderDetail({ cores: 4 });
  assert.equal(cpuDetail, '4 vCPU');
  const half = await renderDetail({ cores: 3.5 });
  assert.equal(half.cpuDetail, '3.5 vCPU');
});

test('an unknown core count shows "—" without the stray vCPU unit', async () => {
  const { cpuDetail } = await renderDetail({ cores: null });
  assert.equal(cpuDetail, '—', 'no core count must not read "— vCPU"');
});

test('System info lists one row per node address, then the existing rows', async () => {
  const { kvpRows } = await renderDetail({
    addresses: [
      { type: 'InternalIP', address: '10.78.0.11' },
      { type: 'Hostname', address: 'ske-e2e-ui-cp1' },
    ],
  });
  const keys = kvpRows.map(([k]) => k);
  assert.ok(keys.includes('InternalIP'), `InternalIP row missing (${keys.join(', ')})`);
  assert.ok(keys.includes('Hostname'), `Hostname row missing (${keys.join(', ')})`);
  assert.deepEqual(kvpRows.filter(([k]) => k === 'InternalIP' || k === 'Hostname'), [
    ['InternalIP', '10.78.0.11'], ['Hostname', 'ske-e2e-ui-cp1'],
  ], 'address values pass through in API order');
  assert.ok(keys.includes('Instance type'), 'the existing Instance type row stays');
});

test('a node without addresses adds no empty System info rows', async () => {
  const { kvpRows } = await renderDetail({});
  const keys = kvpRows.map(([k]) => k);
  assert.equal(keys.includes('InternalIP'), false, 'no InternalIP row without an address');
  assert.equal(keys.includes('Hostname'), false, 'no Hostname row without an address');
});
