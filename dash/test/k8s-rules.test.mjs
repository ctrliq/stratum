import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';

// Regression guard: the object rules the loader and the resource
// registry both need live in ONE DOM-free module, src/k8s-rules.js. This file
// pins the module's exports over the same fixtures rules-parity.test.mjs runs
// both paths over, then reads the modules that consume them (live.js,
// resources.jsx, k8sutil.js) to prove no second definition exists — a
// second `function podStatus` two files away means two rules drifting apart.
// Importing or re-exporting the shared rules is fine; defining a second copy
// is not.

const rules = await import('../src/k8s-rules.js');

// The same fixtures rules-parity.test.mjs serves to both paths, so a value
// pinned here and a value pinned there are about the same objects.
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
// 'once' carries no spec.completions: the status rule defaults it to 1.
const jobs = [
  job('done', { succeeded: 2 }, 2),
  job('broke', { failed: 1 }, 1),
  job('working', { active: 1 }, 1),
  job('queued', {}, 1),
  job('two-of-three', { succeeded: 2, active: 1 }, 3),
  job('once', { succeeded: 1 }, undefined),
];
const jobFixture = Object.fromEntries(jobs.map((j) => [j.metadata.name, j]));

// Mid-rollout: 3 desired, every replica ready, only 1 available. Ready reads
// readyReplicas (3/3, what kubectl get deployments prints), NOT availability.
const dep = { metadata: { name: 'api', namespace: 'app', creationTimestamp: hoursAgo(3) },
  spec: { replicas: 3, template: { spec: { containers: [{ image: 'api:v1' }] } } },
  status: { readyReplicas: 3, updatedReplicas: 3, availableReplicas: 1 } };

test('the shared object rules give the pinned values over the fixtures', async (t) => {
  // Pod status and ready count, for every branch of both rules.
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
    const p = podFixture[name];
    assert.ok(p, name);
    assert.equal(rules.podStatus(p), status, `podStatus for ${name}`);
    assert.equal(rules.podReady(p), ready, `podReady for ${name}`);
  }

  // Job status: complete, failed, active, not started, part-way to its
  // completions, and the completions default of 1.
  for (const [name, status] of [
    ['done', 'Complete'], ['broke', 'Failed'], ['working', 'Running'],
    ['queued', 'Scheduled'], ['two-of-three', 'Running'], ['once', 'Complete'],
  ]) {
    assert.equal(rules.jobStatus(jobFixture[name]), status, `jobStatus for ${name}`);
  }

  // The one Deployment Ready rule: readyReplicas over replicas — the kubectl
  // convention users know — never availableReplicas.
  assert.equal(rules.workloadReady(dep), '3/3', 'workloadReady reads readyReplicas over replicas');
  assert.equal(rules.workloadReady({ spec: { replicas: 3 }, status: { readyReplicas: 1 } }), '1/3',
    'workloadReady mid-rollout');
  assert.equal(rules.workloadReady({ spec: { replicas: 3 } }), '0/3', 'workloadReady with no status yet');
  assert.equal(rules.workloadReady({}), '0/0', 'workloadReady with neither field');
  // StatefulSets carry the same field pair, so the same rule answers for them.
  assert.equal(rules.workloadReady({ spec: { replicas: 2 }, status: { readyReplicas: 2 } }), '2/2',
    'workloadReady over a StatefulSet');

  // Relative age. The clock is mocked so the buckets cannot drift under the run.
  t.mock.timers.enable({ apis: ['Date'] });
  t.mock.timers.setTime(1700000000000);
  const ts = (secondsAgo) => new Date(1700000000000 - secondsAgo * 1000).toISOString();
  assert.equal(rules.age(null), '—', 'age of no timestamp');
  assert.equal(rules.age(ts(30)), '30s', 'age of a 30s-old timestamp');
  assert.equal(rules.age(ts(3 * 3600)), '3h', 'age of a 3h-old timestamp');
  assert.equal(rules.age(ts(3 * 86400)), '3d', 'age of a 3d-old timestamp');
  t.mock.timers.reset();

  // Quantities: the same values k8sutil.test.mjs pins on the re-exports.
  for (const [q, millicores] of [
    ['250m', 250], ['2', 2000], [0.5, 500], ['1500000n', 1.5], ['500u', 0.5], [null, 0],
  ]) {
    assert.equal(rules.cpuToM(q), millicores, `cpuToM(${String(q)})`);
  }
  for (const [q, mib] of [
    ['512Mi', 512], ['1Gi', 1024], ['1024Ki', 1], [null, 0], ['not-a-quantity', 0],
  ]) {
    assert.equal(rules.memToMi(q), mib, `memToMi(${String(q)})`);
  }
  assert.equal(Number(rules.memToMi('1M').toFixed(6)), 0.953674, 'memToMi(1M)');
});

// A re-export (`export { age } from './k8s-rules.js'`) or a plain import is
// allowed; the scan matches only a definition. Source-text scanning is
// heuristic lint, not proof — rules-parity.test.mjs holds the behavioural half
// of this guard over both paths at once.
test('each shared rule is defined once', () => {
  const names = 'podStatus|podPhase|podReady|readyStr|podReadyStr|jobStatus|workloadReady|age|cpuToM|memToMi';
  const defines = new RegExp(
    `(?:^|\\n)\\s*(?:export\\s+)?(?:async\\s+)?function\\s+(?:${names})\\b` +
    `|(?:^|\\n)\\s*(?:export\\s+)?const\\s+(?:${names})\\b`);
  for (const file of ['live.js', 'resources.jsx', 'k8sutil.js']) {
    const src = readFileSync(new URL(`../src/${file}`, import.meta.url), 'utf8');
    const hit = src.match(defines);
    assert.equal(hit, null, `${file} defines a second copy of a shared rule: ${hit && hit[0].trim()}`);
  }
});
