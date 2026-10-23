import assert from 'node:assert/strict';
import test from 'node:test';
import { harness, text } from './harness.mjs';

// Regression guards: the loader (live.js, which fills the K8S
// object the hand-written screens read) and the resource registry's column
// functions (resources.jsx, which the generic kind lists render) read the same
// objects through two paths. Both derive every shared cell from the one module
// (src/k8s-rules.js); these tests pin both sides over the same fixtures so any
// drift fails. The Deployment Ready cell is pinned CONVERGED: READY reads
// readyReplicas over replicas — the kubectl convention users know — on the
// Workloads screen and in the Deployments list alike, while the row's status
// keeps reading availableReplicas, because Ready and Available answer
// different questions.

// live.js talks to the apiserver through the global fetch and stamps window on
// success, so both have to exist before it is imported.
globalThis.window = {};
const api = { routes: {} };
globalThis.fetch = async (url) => {
  const path = String(url).replace(/^\/k8s/, '');
  const hit = api.routes[path];
  if (hit === undefined) return { ok: false, status: 404, json: async () => null };
  return { ok: true, status: 200, json: async () => hit };
};
console.warn = () => {}; console.log = () => {};

const { loadLiveK8S } = await import('../src/live.js');
const { K8S, resetK8S } = await import('../src/data.js');

const list = (...items) => ({ items });
const now = Date.now();
const hoursAgo = (h) => new Date(now - h * 3600e3).toISOString();
const cstat = (ready, state = { running: {} }) => ({ name: 'c', ready, state, restartCount: 0 });
const waiting = (reason) => cstat(false, { waiting: { reason } });

// Pods cover every branch of the status and ready rules: all containers ready,
// one of two ready, CrashLoopBackOff, the two waiting reasons that read Failed,
// a deletionTimestamp, no status at all, and a Succeeded phase.
const pods = [
  { metadata: { name: 'all-ready', namespace: 'app', creationTimestamp: hoursAgo(3) },
    spec: { nodeName: 'w-1', containers: [{ resources: { requests: { cpu: '250m', memory: '512Mi' } } }, {}] },
    status: { phase: 'Running', podIP: '10.0.0.1', containerStatuses: [cstat(true), cstat(true)] } },
  { metadata: { name: 'half-ready', namespace: 'app', creationTimestamp: hoursAgo(3) },
    spec: { nodeName: 'w-1', containers: [{}, {}] },
    status: { phase: 'Running', containerStatuses: [cstat(true), cstat(false)] } },
  { metadata: { name: 'crashloop', namespace: 'app', creationTimestamp: hoursAgo(3) },
    spec: { nodeName: 'w-1', containers: [{}] },
    status: { phase: 'Running', containerStatuses: [waiting('CrashLoopBackOff')] } },
  ...['ImagePullBackOff', 'CreateContainerConfigError'].map((reason) => ({
    metadata: { name: reason, namespace: 'app', creationTimestamp: hoursAgo(3) },
    spec: { nodeName: 'w-1', containers: [{}] },
    status: { phase: 'Pending', containerStatuses: [waiting(reason)] },
  })),
  { metadata: { name: 'terminating', namespace: 'app', creationTimestamp: hoursAgo(3), deletionTimestamp: hoursAgo(0.1) },
    spec: { nodeName: 'w-1', containers: [{}] },
    status: { phase: 'Running', containerStatuses: [cstat(true)] } },
  { metadata: { name: 'no-status', namespace: 'app', creationTimestamp: hoursAgo(3) },
    spec: { nodeName: 'w-1', containers: [{}] } },
  { metadata: { name: 'succeeded', namespace: 'app', creationTimestamp: hoursAgo(3) },
    spec: { nodeName: 'w-1', containers: [{}] },
    status: { phase: 'Succeeded', containerStatuses: [cstat(false)] } },
];
const podFixture = Object.fromEntries(pods.map((p) => [p.metadata.name, p]));

const job = (name, status, completions) => ({ metadata: { name, namespace: 'app', creationTimestamp: hoursAgo(3) },
  spec: { completions, template: { spec: { containers: [{ image: 'job:v1' }] } } }, status });
// 'once' carries no spec.completions: the status and completions rules
// default it to 1.
const jobs = [
  job('done', { succeeded: 2 }, 2),
  job('broke', { failed: 1 }, 1),
  job('working', { active: 1 }, 1),
  job('queued', {}, 1),
  job('two-of-three', { succeeded: 2, active: 1 }, 3),
  job('once', { succeeded: 1 }, undefined),
];
const jobFixture = Object.fromEntries(jobs.map((j) => [j.metadata.name, j]));

const dep = { metadata: { name: 'api', namespace: 'app', creationTimestamp: hoursAgo(3) },
  spec: { replicas: 3, template: { spec: { containers: [{ image: 'api:v1' }] } } },
  status: { readyReplicas: 3, updatedReplicas: 3, availableReplicas: 1 } };
// A second deployment, fully rolled out: every replica ready AND available, so
// the two answers (Ready, Available) agree and the status reads Running.
const depReady = { metadata: { name: 'web', namespace: 'app', creationTimestamp: hoursAgo(3) },
  spec: { replicas: 2, template: { spec: { containers: [{ image: 'web:v1' }] } } },
  status: { readyReplicas: 2, updatedReplicas: 2, availableReplicas: 2 } };

// A cluster that answers everything (no node usage summary, so pod rows price
// their requests), so the test only states the fixtures.
api.routes = {
  '/version': { gitVersion: 'v1.34.1' },
  '/api/v1/nodes': list(), '/api/v1/pods': list(...pods), '/api/v1/namespaces': list(),
  '/api/v1/services': list(),
  '/apis/apps/v1/deployments': list(dep, depReady), '/apis/apps/v1/statefulsets': list(),
  '/apis/apps/v1/daemonsets': list(), '/apis/batch/v1/jobs': list(...jobs), '/apis/batch/v1/cronjobs': list(),
  '/apis/networking.k8s.io/v1/ingresses': list(),
  '/api/v1/persistentvolumeclaims': list(), '/api/v1/persistentvolumes': list(),
  '/apis/storage.k8s.io/v1/storageclasses': list(), '/api/v1/configmaps': list(), '/api/v1/secrets?fieldSelector=type%3Dhelm.sh%2Frelease.v1&includeObject=Metadata': { rows: [] },
  '/api/v1/serviceaccounts': list(), '/apis/rbac.authorization.k8s.io/v1/clusterroles': list(),
  '/apis/rbac.authorization.k8s.io/v1/roles': list(), '/apis/rbac.authorization.k8s.io/v1/clusterrolebindings': list(),
  '/apis/rbac.authorization.k8s.io/v1/rolebindings': list(), '/api/v1/events': list(),
  '/apis/coordination.k8s.io/v1/namespaces/kube-system/leases/stratum-operator': 404,
};

// The REAL descriptor table, with the real k8sutil helpers under it — a stub
// with an empty RESOURCES would prove nothing about either side, and the
// registry's age column is one of the shared rules being pinned. The shared
// rules module rides along real too: the descriptors import it, and a stub
// here would let a second copy come back inside resources.jsx unnoticed.
const h = await harness({ real: { '/resources.jsx': 'resources.jsx', '/k8sutil.js': 'k8sutil.js', '/k8s-rules.js': 'k8s-rules.js' } });
const { RESOURCES } = await h.module('resources.jsx');
const col = (desc, key) => desc.columns.find((c) => c.key === key).render;

test('the loader and the kind lists agree on every shared rule', async () => {
  resetK8S();
  await loadLiveK8S();

  // Pods: each row the loader writes and each cell the registry renders come
  // from their own copy of the rules, over the same fixture object.
  for (const [name, status, ready] of [
    ['all-ready', 'Running', '2/2'],
    ['half-ready', 'Running', '1/2'],
    ['crashloop', 'CrashLoopBackOff', '0/1'],
    ['ImagePullBackOff', 'Failed', '0/1'],
    ['CreateContainerConfigError', 'Failed', '0/1'],
    ['terminating', 'Terminating', '1/1'],
    ['no-status', 'Pending', '0/1'],
    ['succeeded', 'Succeeded', '0/1'],
  ]) {
    const row = K8S.PODS.find((r) => r.name === name);
    const o = podFixture[name];
    assert.ok(row && o, name);
    assert.equal(row.status, status, `loader podStatus for ${name}`);
    assert.equal(row.ready, ready, `loader readyStr for ${name}`);
    assert.equal(col(RESOURCES.pods, 'status')(o).props.status, status, `registry status pill for ${name}`);
    assert.equal(text(col(RESOURCES.pods, 'ready')(o)), ready, `registry ready cell for ${name}`);
  }

  // Jobs: the loader's Workloads rows and the registry's completions and
  // status columns over the same fixtures.
  for (const [name, status, completions] of [
    ['done', 'Complete', '2/2'],
    ['broke', 'Failed', '0/1'],
    ['working', 'Running', '0/1'],
    ['queued', 'Scheduled', '0/1'],
    ['two-of-three', 'Running', '2/3'],
    ['once', 'Complete', '1/1'],
  ]) {
    const row = K8S.WORKLOADS.find((w) => w.kind === 'Job' && w.name === name);
    const o = jobFixture[name];
    assert.ok(row && o, name);
    assert.equal(row.status, status, `loader Job status for ${name}`);
    assert.equal(row.ready, completions, `loader Job ready for ${name}`);
    assert.equal(text(col(RESOURCES.jobs, 'completions')(o)), completions, `registry completions cell for ${name}`);
    assert.equal(col(RESOURCES.jobs, 'status')(o).props.status, status, `registry Job pill for ${name}`);
  }

  // Age and quantities: three hours sits mid-bucket, so neither side can
  // straddle a boundary; no usage summary answered, so the row prices requests.
  const allReady = K8S.PODS.find((r) => r.name === 'all-ready');
  assert.equal(allReady.age, '3h', 'loader age');
  assert.equal(text(col(RESOURCES.pods, 'age')(podFixture['all-ready'])), '3h', 'registry age cell');
  assert.equal(allReady.cpu, 250, 'loader cpu from the request');
  assert.equal(allReady.mem, 512, 'loader mem from the request');
});

// The Deployment Ready rule is decided ONCE, in k8s-rules.js: a
// Deployment shows readyReplicas over replicas — what kubectl get deployments
// prints — on the Workloads screen and in its list. The row's status
// reads available replicas, so a mid-rollout Deployment reads 3/3 and
// Progressing: Ready and Available answer different questions.
test('a Deployment shows one Ready count on the Workloads screen and in its list', async () => {
  resetK8S();
  await loadLiveK8S();
  const depRow = K8S.WORKLOADS.find((w) => w.kind === 'Deployment' && w.name === 'api');
  assert.equal(depRow.ready, '3/3', 'loader Deployment Ready reads readyReplicas');
  assert.equal(depRow.status, 'Progressing', 'the loader status keeps reading available replicas');
  assert.equal(text(col(RESOURCES.deployments, 'ready')(dep)), '3/3', 'the Deployments list renders readyReplicas');

  // Every replica ready AND available: the two answers agree, and the status
  // the Workloads screen shows reads Running.
  const rolledRow = K8S.WORKLOADS.find((w) => w.kind === 'Deployment' && w.name === 'web');
  assert.equal(rolledRow.ready, '2/2', 'loader Deployment Ready over a fully rolled-out Deployment');
  assert.equal(rolledRow.status, 'Running', 'a Deployment with every replica available reads Running');
  assert.equal(text(col(RESOURCES.deployments, 'ready')(depReady)), '2/2', 'the Deployments list agrees');
  // Availability keeps its own answer, one column over — it is not folded
  // into Ready.
  assert.equal(text(col(RESOURCES.deployments, 'avail')(dep)), '1', 'the Available column still reads availableReplicas');
});
