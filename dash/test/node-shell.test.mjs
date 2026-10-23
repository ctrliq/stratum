import assert from 'node:assert/strict';
import test from 'node:test';
import fs from 'node:fs/promises';
import vm from 'node:vm';

// ============================================================
// node-shell.test.mjs — the node host shell's lifecycle as a batch/v1 Job.
//
// The shell is a batch/v1 Job the TTL controller owns, not a bare pod the SPA
// has to remember to delete. The create POSTs a Job whose template carries
// the same privileged hostPID pod, spec.ttlSecondsAfterFinished makes the TTL
// controller delete the finished Job, and a Job-level
// spec.activeDeadlineSeconds bounds a Job whose
// pod never starts. Every close path — drawer close, pagehide, sign-out —
// deletes the JOB as best-effort acceleration of that same teardown, never a
// pod, and the exec target stays the Job-derived pod: onPod reports its name
// for the drawer's exec WebSocket.
//
// The interesting close is still the drawer closing while the create POST is
// in flight: the stop function runs before the Job even has a name, so the
// only half that can delete it is the POST's own continuation. The tab
// closing is the one stop the page cannot make, so the module registers a
// pagehide handler that sends a best-effort keepalive delete for each open
// Job.
// ============================================================
const JOB = 'stratum-node-shell-abc12';         // the Job object the create answers with
const POD = 'stratum-node-shell-abc12-9f2kx';   // the Job-derived pod the exec targets
const IMAGE = 'ghcr.io/ctrliq/stratum/busybox:1.37.0'; // a server-supplied payload image
// What the apiserver answers for the create: the object carries the selector
// the batch controller stamps at create time, which is how the SPA finds the
// pod the Job made.
const JOB_OBJECT = {
  metadata: { name: JOB },
  spec: { selector: { matchLabels: { 'job-name': JOB } } },
};
const calls = [];
const inits = [];         // the fetch init each call carried (headers for the CSRF pin)
let lastPostBody = null;  // the create Job spec, as k8sutil sent it (JSON text)
let releaseCreate = null; // resolved by the test to let the create POST answer
let phase = 'Pending';    // what the pod's status reports back
let deleteOk = true;      // whether the apiserver accepts the DELETE
let failDeletes = false;  // whether the DELETE fetch itself rejects (the network is gone)

// node-shell.js registers its unload cleanup on window, so window has to exist
// — with a recording addEventListener — before the module is imported, the way
// live.test.mjs stands in for window before importing live.js.
const pagehide = []; // the pagehide handlers the module registered
globalThis.window = {
  addEventListener: (type, fn) => { if (type === 'pagehide') pagehide.push(fn); },
};

// The Job's status, as the apiserver derives it from the pod's phase: both
// readiness surfaces (the pod's phase, the Job's counts) answer, so the tests
// pin behaviour rather than which one the module reads.
const jobStatus = () => {
  if (phase === 'Running') return { active: 1, ready: 1 };
  if (phase === 'Failed') return { failed: 1 };
  if (phase === 'Succeeded') return { succeeded: 1 };
  return { active: 1 };
};

globalThis.fetch = async (url, opts = {}) => {
  const u = String(url);
  const method = opts.method || 'GET';
  calls.push(method + ' ' + u);
  inits.push(opts);
  // The writes go through k8sutil's send(), which reads the body as text.
  if (method === 'POST' && u.endsWith('/apis/batch/v1/namespaces/default/jobs')) {
    lastPostBody = opts.body;
    await new Promise((r) => { releaseCreate = r; });
    return { ok: true, status: 201, text: async () => JSON.stringify(JOB_OBJECT) };
  }
  if (method === 'DELETE') {
    if (failDeletes) throw new Error('network gone');
    return deleteOk ? { ok: true, status: 200, text: async () => '{}' } : { ok: false, status: 500, text: async () => 'boom' };
  }
  if (method !== 'GET') return { ok: false, status: 405, text: async () => '' };
  const podObject = { metadata: { name: POD, labels: { 'job-name': JOB } }, status: { phase } };
  if (/\/jobs\/[^/?]+$/.test(u)) {
    return { ok: true, status: 200, text: async () => JSON.stringify({ ...JOB_OBJECT, status: jobStatus() }) };
  }
  if (/\/jobs(\?|$)/.test(u)) {
    return { ok: true, status: 200, text: async () => JSON.stringify({ items: [{ ...JOB_OBJECT, status: jobStatus() }] }) };
  }
  if (/\/pods\/[^/?]+$/.test(u)) {
    return { ok: true, status: 200, text: async () => JSON.stringify(podObject) };
  }
  if (/\/pods(\?|$)/.test(u)) {
    return { ok: true, status: 200, text: async () => JSON.stringify({ items: [podObject] }) };
  }
  return { ok: true, status: 200, text: async () => JSON.stringify({ status: { phase } }) };
};
console.warn = () => {}; // k8sGet warns on a failed poll

const { startNodeShell } = await import('../src/node-shell.js');
const { setCsrfToken } = await import('../src/session.js');

const settle = async () => { for (let i = 0; i < 20; i++) await Promise.resolve(); };
const deletes = () => calls.filter((c) => c.startsWith('DELETE'));
function begin(image = IMAGE) {
  calls.length = 0; inits.length = 0; lastPostBody = null; releaseCreate = null; phase = 'Pending'; deleteOk = true; failDeletes = false;
  const seen = [];
  const stop = startNodeShell('w-1', image, {
    onPod: (name) => seen.push('pod:' + name), onReady: () => seen.push('ready'), onError: (m) => seen.push('err:' + m),
  });
  return { stop, seen };
}
// The status poll sits behind a real 1500ms setTimeout, so the tests that need
// it fake the clock rather than wait; tick generously, the delay is an interval
// the shell is free to change.
const poll = async (t) => { t.mock.timers.tick(5000); await settle(); };
// Fire the pagehide handler the way the browser fires it as the tab goes away,
// and await what it returns so a failure the unload path failed to swallow
// surfaces here as a test failure instead of vanishing.
const firePagehide = async () => {
  assert.equal(pagehide.length, 1, 'exactly one pagehide handler, registered once');
  const r = pagehide[0]();
  if (r && typeof r.then === 'function') await r;
  await settle();
};

test('closing the drawer mid-create still deletes the Job', async () => {
  const { stop, seen } = begin();
  await settle();
  assert.ok(calls.some((c) => c.startsWith('POST')), 'the create POST is in flight');

  stop();                 // the drawer closes: no Job name exists yet
  releaseCreate();        // ...and only now does the apiserver answer
  await settle();

  assert.deepEqual(deletes(), [`DELETE /k8s/apis/batch/v1/namespaces/default/jobs/${JOB}?gracePeriodSeconds=1&propagationPolicy=Background`],
    'a privileged hostPID Job was left on the node with nothing to end it early');
  assert.deepEqual(seen, [], 'a stopped shell reports nothing back to the unmounted component');
});

test('closing it once the shell is up deletes the Job exactly once', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const { stop, seen } = begin();
  await settle();
  phase = 'Running';
  releaseCreate();
  await settle();
  await poll(t);
  assert.deepEqual(seen, ['pod:' + POD, 'ready'],
    'onPod reports the Job-derived pod the drawer execs into, then the shell reaches the terminal');

  stop();
  await settle();
  assert.equal(deletes().length, 1, 'one delete, not one per half');
  assert.ok(deletes()[0].endsWith(`/jobs/${JOB}?gracePeriodSeconds=1&propagationPolicy=Background`),
    'the teardown deletes the Job, not the pod — the TTL controller owns the pod that goes with it');
});

test('a Job whose pod fails to start is deleted, not left for the error panel to hold', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const { stop, seen } = begin();
  await settle();
  phase = 'Failed';
  releaseCreate();
  await settle();
  await poll(t);

  const errs = seen.filter((s) => s.startsWith('err:'));
  assert.equal(errs.length, 1, 'exactly one failure is reported');
  assert.match(errs[0], /Failed/, 'the failure names the phase the pod died in');
  assert.equal(deletes().length, 1, 'the error state does not keep a privileged Job alive');
  assert.ok(deletes()[0].endsWith(`/jobs/${JOB}?gracePeriodSeconds=1&propagationPolicy=Background`), 'the error path deletes the Job');

  stop();
  await settle();
  assert.equal(deletes().length, 1, 'and closing the panel afterwards does not repeat a DELETE that landed');
});

test('a DELETE the apiserver refuses is retried when the panel closes', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const { stop } = begin();
  await settle();
  phase = 'Failed'; deleteOk = false;
  releaseCreate();
  await settle();
  await poll(t);
  assert.equal(deletes().length, 1, 'the error path tries once');

  deleteOk = true;
  stop();
  await settle();
  assert.equal(deletes().length, 2, 'a failed delete must not strand the Job — the name is kept for the retry');
  assert.ok(deletes().every((c) => c.includes(`/jobs/${JOB}`)), 'both attempts target the Job');
});

test('the debug Job runs the server-provided image, not a hard-coded one', async () => {
  const { stop } = begin();
  await settle();
  assert.ok(lastPostBody, 'the create POST carried a body');
  const body = JSON.parse(lastPostBody);
  assert.equal(body.spec.template.spec.containers[0].image, IMAGE,
    'the Job template must use the image the server supplied');
  stop(); releaseCreate(); await settle();
});

test('a missing image fails closed — no Job is created', async () => {
  const { stop, seen } = begin('');
  await settle();
  assert.equal(calls.filter((c) => c.startsWith('POST')).length, 0, 'no debug Job is created without a server image');
  assert.equal(seen.length, 1, 'exactly one callback fired');
  assert.match(seen[0], /^err:.*image/, 'the failure is reported through onError, mentioning the missing image');
  stop();
});

test('a missing image stopped before the deferred error stays silent', async () => {
  // The fail-closed onError is deferred to a microtask; stopping first (an
  // unmount) must suppress it, like every other callback (contract: nothing
  // fires after stop()).
  const { stop, seen } = begin('');
  stop();
  await settle();
  assert.deepEqual(seen, [], 'a stopped shell reports nothing, even on the fail-closed path');
  assert.equal(calls.filter((c) => c.startsWith('POST')).length, 0, 'and it still creates no Job');
});

test('denied shell creates release their retained lifecycle callbacks', async () => {
  // Observe retained functions in the module's collections without adding a
  // production inspection API. A failed create has no Job to clean up later.
  const collections = [];
  class RecordingSet extends Set {
    constructor(...args) { super(...args); collections.push(this); }
  }
  const context = vm.createContext({ Set: RecordingSet,
    window: { addEventListener() {} }, console });
  const source = await fs.readFile(new URL('../src/node-shell.js', import.meta.url), 'utf8');
  const module = new vm.SourceTextModule(source, { context });
  const transport = new vm.SyntheticModule(['k8sGet', 'k8sPost', 'k8sFetch'], function () {
    this.setExport('k8sGet', async () => null);
    this.setExport('k8sPost', async () => ({ ok: false, status: 403, error: 'forbidden' }));
    this.setExport('k8sFetch', async () => ({ ok: true, status: 200 }));
  }, { context });
  const session = new vm.SyntheticModule(['currentSessionRevision', 'subscribeSessionChanges'], function () {
    this.setExport('currentSessionRevision', () => 0);
    this.setExport('subscribeSessionChanges', () => () => {});
  }, { context });
  await module.link((specifier) => specifier.endsWith('/session.js') ? session : transport);
  await module.evaluate();
  const errors = [];
  for (let i = 0; i < 3; i++) {
    const stop = module.namespace.startNodeShell('w-1', IMAGE, {
      onPod() {}, onReady() {}, onError: (message) => errors.push(message),
    });
    await settle();
    stop();
  }
  assert.equal(errors.length, 3, 'each denied attempt is reported');
  const retainedCallbacks = collections.flatMap((set) => [...set]).filter((value) => typeof value === 'function');
  assert.equal(retainedCallbacks.length, 0, 'failed opens must not retain unload closures until logout');
});

test('the create POSTs a batch/v1 Job whose template keeps the fields the server gate keys on', async () => {
  const { stop } = begin();
  await settle();
  const posts = calls.filter((c) => c.startsWith('POST'));
  assert.deepEqual(posts, ['POST /k8s/apis/batch/v1/namespaces/default/jobs'],
    'the create is one POST to the default-namespace batch jobs');

  const body = JSON.parse(lastPostBody);
  assert.equal(body.apiVersion, 'batch/v1', 'the shell is a batch/v1 Job');
  assert.equal(body.kind, 'Job');
  // The gate reads the template's name prefix (internal/dashboard/nodeshell.go):
  // a rename would let an exec reach a leftover shell pod after a restart with
  // the shell off.
  assert.equal(body.metadata.generateName, 'stratum-node-shell-');
  // The template's labels land on the pod the Job creates.
  assert.equal(body.spec.template.metadata.labels.app, 'stratum-node-shell');
  assert.equal(body.spec.template.metadata.labels['stratum.ciq.com/node-shell'], 'w-1');
  assert.equal(body.spec.template.spec.nodeName, 'w-1');
  // The gate reads the template's hostPID, and the nsenter exec needs the host
  // process namespace.
  assert.equal(body.spec.template.spec.hostPID, true);
  assert.equal(body.spec.template.spec.restartPolicy, 'Never');
  assert.deepEqual(body.spec.template.spec.tolerations, [{ operator: 'Exists' }]);
  assert.equal(body.spec.template.spec.containers.length, 1, 'exactly one container');
  const container = body.spec.template.spec.containers[0];
  assert.equal(container.name, 'shell');
  // The shell still runs `sleep`; how long it may sleep is pinned against the
  // pod's deadline by "the Job's life is bounded in the cluster".
  assert.equal(container.command[0], 'sleep');
  assert.equal(container.securityContext.privileged, true);
  assert.equal(container.stdin, true);
  assert.equal(container.tty, true);

  stop(); releaseCreate(); await settle();
});

test("the Job's life is bounded in the cluster and the TTL controller retires it", async () => {
  const { stop } = begin();
  await settle();
  assert.ok(lastPostBody, 'the create POST carried a body');
  const body = JSON.parse(lastPostBody);
  // ttlSecondsAfterFinished lets the TTL controller delete
  // the finished Job — its pod with it — with no session or sweep needed, so a
  // shell that outlives every later session does not stay on the object
  // list forever.
  const ttl = body.spec.ttlSecondsAfterFinished;
  assert.ok(Number.isInteger(ttl) && ttl > 0, 'spec.ttlSecondsAfterFinished is a positive integer the TTL controller acts on');
  // The Job-level activeDeadlineSeconds bounds the Job itself: a pod that
  // never starts (an unschedulable node) would otherwise hold the object open
  // past any session, because the pod's own deadline never begins.
  const jobDeadline = body.spec.activeDeadlineSeconds;
  assert.ok(Number.isInteger(jobDeadline) && jobDeadline > 0, 'spec.activeDeadlineSeconds is a positive integer');
  // The template keeps the pod-level deadline: the kubelet bounds active
  // execution from the pod's start time, independently of the tab. It bounds
  // the longest host-shell session wanted, so it is no longer than today's
  // hour-long sleep, and the container must not outlive the window it runs in.
  const podDeadline = body.spec.template.spec.activeDeadlineSeconds;
  assert.ok(Number.isInteger(podDeadline) && podDeadline > 0, 'the template carries a positive activeDeadlineSeconds');
  assert.ok(podDeadline <= 3600, 'the pod deadline is no longer than the sleep it replaces');
  assert.deepEqual(body.spec.template.spec.containers[0].command, ['sleep', String(podDeadline)],
    'the container sleeps exactly as long as the deadline bounds the pod');
  assert.ok(jobDeadline >= podDeadline,
    'the Job deadline must not be shorter than the pod deadline it sits above');
  // backoffLimit 0 keeps the one-shell-per-create contract the bare pod had:
  // the default (6) would let the Job controller quietly replace a failed
  // shell pod with another privileged hostPID pod the drawer never execs
  // into. A failed pod fails the Job, and the TTL controller owns cleanup.
  assert.equal(body.spec.backoffLimit, 0,
    'a failed shell pod must finish the Job, not be replaced by another privileged pod');
  stop(); releaseCreate(); await settle();
});

test('the Job is deleted with its grace period and the CSRF header', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  setCsrfToken('csrf');
  try {
    const { stop } = begin();
    await settle();
    phase = 'Running';
    releaseCreate();
    await settle();
    await poll(t);

    stop();
    await settle();
    const sent = calls.map((call, i) => ({ call, init: inits[i] })).filter((r) => r.call.startsWith('DELETE'));
    assert.equal(sent.length, 1, 'closing the shell sends exactly one delete');
    assert.equal(sent[0].call, `DELETE /k8s/apis/batch/v1/namespaces/default/jobs/${JOB}?gracePeriodSeconds=1&propagationPolicy=Background`,
      'the delete is the Job object, and it carries its one-second grace period');
    assert.equal(sent[0].init.headers['X-Stratum-CSRF'], 'csrf',
      'the delete carries the CSRF header the proxy requires on every write');
  } finally {
    setCsrfToken(null);
  }
});

test("closing the tab sends each open shell's Job delete as a keepalive request", async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  setCsrfToken('csrf');
  try {
    const { stop } = begin();
    await settle();
    phase = 'Running';
    releaseCreate();
    await settle();
    await poll(t);

    // The tab going away is the one close the page cannot perform, so the
    // module registers a pagehide handler that deletes every open shell
    // best-effort. It goes out as a fetch with keepalive — not
    // navigator.sendBeacon, which cannot carry the X-Stratum-CSRF header the
    // proxy requires on every write — and takes the same form as the
    // drawer-close delete: against the Job, accelerating the TTL teardown.
    await firePagehide();
    const sent = calls.map((call, i) => ({ call, init: inits[i] })).filter((r) => r.call.startsWith('DELETE'));
    assert.equal(sent.length, 1, 'one delete for the one open shell');
    assert.equal(sent[0].call, `DELETE /k8s/apis/batch/v1/namespaces/default/jobs/${JOB}?gracePeriodSeconds=1&propagationPolicy=Background`,
      'the unload delete takes the same form as the drawer-close delete');
    assert.equal(sent[0].init.keepalive, true,
      'the delete rides a keepalive request, the only delivery the browser may still attempt');
    assert.equal(sent[0].init.headers['X-Stratum-CSRF'], 'csrf',
      'the unload delete carries the CSRF header the proxy requires on every write');

    stop();
    await settle();
    const afterStop = deletes().length;
    await firePagehide();
    assert.equal(deletes().length, afterStop,
      'a shell the user had already closed sends nothing on pagehide');
  } finally {
    setCsrfToken(null);
  }
});

test('a pagehide delete that fails is swallowed, not surfaced', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  setCsrfToken('csrf');
  try {
    const { stop } = begin();
    await settle();
    phase = 'Running';
    releaseCreate();
    await settle();
    await poll(t);

    // Best-effort means every failure mode is ignored: the browser may be
    // tearing the page down mid-request (a rejecting fetch) and the apiserver
    // may refuse what does arrive. Neither may throw out of the handler or
    // leave an unhandled rejection — the page is going away either way.
    failDeletes = true;
    await firePagehide();
    failDeletes = false; deleteOk = false;
    await firePagehide();
    deleteOk = true;
    assert.equal(deletes().length, 2, 'both best-effort attempts were made, and neither escaped');
    assert.ok(deletes().every((c) => c.includes(`/jobs/${JOB}`)), 'both unload attempts target the Job');

    stop();
    await settle();
    assert.ok(deletes().length >= 2, 'the shell still tears down normally after the failed unload deletes');
  } finally {
    setCsrfToken(null);
  }
});
