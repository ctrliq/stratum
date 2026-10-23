import assert from 'node:assert/strict';
import test from 'node:test';
import { harness, collect } from './harness.mjs';

// A react stub whose useState answers per-slot overrides by call index, so a
// test can inject state (the selected object) a live component would only get
// through user interaction, and whose setters record the writes they are
// offered so an effect body can be exercised without a renderer.
//
// ResourceList's useState slots are POSITIONAL: 0 items, 1 loading, 2 err,
// 3 q, 4 sel, 5 confirm, 6 creating, 7 busy, 8 nonce. A new useState added
// above them shifts every later slot — update these indices if that happens.
// Reset the counter before each render: the slots count per render pass.
const SLOTS = { items: 0, loading: 1, err: 2, q: 3, sel: 4, confirm: 5, creating: 6, busy: 7, nonce: 8 };

function reactStub({ state = {}, effects = null } = {}) {
  const sets = [];
  let slot = 0;
  const react = {
    useState: (initial) => {
      const i = slot++;
      const value = i in state ? state[i] : (typeof initial === 'function' ? initial() : initial);
      return [value, (v) => sets.push([i, v])];
    },
    useEffect: effects ? (fn, deps) => effects.push([fn, deps]) : () => {},
    useRef: () => ({}),
    useCallback: (fn) => fn,
    useMemo: (fn) => fn(),
    useSyncExternalStore: (_subscribe, snapshot) => snapshot(),
  };
  return {
    react, sets, state,
    reset: () => { slot = 0; sets.length = 0; },
  };
}

// The stub set every ResourceList render needs: leaf widgets stay inert, the
// resource search returns a string like the real one (an empty one, so no row
// matches a query), and k8sutil.js runs for real so nameOf, nsOf and meta
// behave as in the app (that is what the object key is built
// from). The counter must be reset before each render.
async function resourceListHarness(r) {
  return harness({ stubs: {
    react: r.react,
    './shell.jsx': { Modal: 'Modal', PaneShell: 'PaneShell' },
    './codeview.jsx': { CodeView: 'CodeView', CodeEditor: 'CodeEditor' },
    '/resources.jsx': { resourceSearch: () => '', instancePath: () => '', createPath: () => '' },
  }, real: { '/k8sutil.js': 'k8sutil.js' } });
}

const descriptor = {
  path: '/apis/apps/v1/deployments', label: 'Deployments', kind: 'Deployment', namespaced: true,
  columns: [{ key: 'name', label: 'Name', render: (o) => o.metadata?.name || '' }],
};

test('the row opens on Enter or Space, but not on a row-action button inside it', async () => {
  const h = await harness({ stubs: {
    './shell.jsx': { Modal: 'Modal', PaneShell: 'PaneShell' },
    './codeview.jsx': { CodeView: 'CodeView', CodeEditor: 'CodeEditor' },
    '/k8sutil.js': { k8sGet: () => {}, k8sDelete: () => {}, k8sPost: () => {}, k8sPut: () => {}, k8sPatch: () => {},
      listItems: () => [], nameOf: () => '', nsOf: () => '', meta: () => ({}), age: () => '', labelPairs: () => [] },
    '/resources.jsx': { resourceSearch: () => true, instancePath: () => '', createPath: () => '' },
  } });
  const { rowActivates } = await h.module('ui/resource-list.jsx');

  const row = { tag: 'tr' }, button = { tag: 'button' };
  for (const key of ['Enter', ' ']) {
    assert.equal(rowActivates({ target: row, currentTarget: row, key }), true);
    // The Logs/Exec buttons are focusable children of the row; their keys bubble
    // up to it and belong to them, not to the row.
    assert.equal(rowActivates({ target: button, currentTarget: row, key }), false);
  }
  for (const key of ['a', 'Tab', 'ArrowDown', 'Escape']) {
    assert.equal(rowActivates({ target: row, currentTarget: row, key }), false);
  }
});

test('the detail pane remounts per selected object, so a pending Scale or Edit cannot carry over', async () => {
  const r = reactStub();
  const h = await resourceListHarness(r);
  const { ResourceList } = await h.module('ui/resource-list.jsx');

  // The third and fourth have no uid and the same name — the namespace is what
  // keeps their keys (and so their pane state) apart. The fifth shares the
  // first's namespace but splits the same characters differently: name 'api'
  // with uid 'u1' versus name 'apiu1' with no uid.
  const objs = [
    { metadata: { namespace: 'a', name: 'api', uid: 'u1' } },
    { metadata: { namespace: 'a', name: 'web', uid: 'u2' } },
    { metadata: { namespace: 'b', name: 'api' } },
    { metadata: { namespace: 'a', name: 'api' } },
    { metadata: { namespace: 'a', name: 'apiu1' } },
  ];
  const expected = ['a/api/u1', 'a/web/u2', 'b/api/', 'a/api/', 'a/apiu1/'];

  const keys = objs.map((obj) => {
    r.reset();
    r.state[SLOTS.sel] = obj;
    const tree = ResourceList({ descriptor, namespace: 'all-namespaces' });
    const [pane] = collect(tree, (n) => n.type?.name === 'DetailPane');
    assert.ok(pane, 'a selected object renders the detail pane');
    return pane.props.key;
  });
  for (const [i, want] of expected.entries()) {
    assert.equal(keys[i], want, `detail pane key for object ${i} (${JSON.stringify(objs[i].metadata)})`);
  }
  // Name and uid need a separator of their own: with none, api + uid u1 and
  // apiu1 with no uid both key 'a/apiu1' and the pane would not remount
  // between them. Kubernetes names and uids cannot contain '/', so it is free.
  assert.notEqual(keys[0], keys[4], 'name+uid u1 and name apiu1 without uid get different keys');
  assert.equal(new Set(keys).size, 5, 'all five keys distinct — namespace matters when the uid is missing');
});

test('switching kind resets the create pane and the confirm dialog along with the rest of the view state', async () => {
  const effects = [];
  const r = reactStub({ effects });
  const h = await resourceListHarness(r);
  const { ResourceList } = await h.module('ui/resource-list.jsx');

  r.reset();
  ResourceList({ descriptor, namespace: 'all-namespaces' });

  const reset = effects.find(([, deps]) => deps.length === 1 && deps[0] === descriptor.path);
  assert.ok(reset, 'the per-kind reset effect is registered once, on [descriptor.path]');
  reset[0]();

  // [slot, value] pairs: open panes and dialogs belong to the previous kind,
  // so creating (slot 6) and confirm (slot 5) reset with q, sel, items,
  // loading and err. Compared as JSON because the [] the effect records was
  // built in the harness's vm realm, whose Array prototype deepStrictEqual
  // cannot match against a literal from this file.
  const expected = [
    [SLOTS.q, ''], [SLOTS.sel, null], [SLOTS.items, []],
    [SLOTS.loading, true], [SLOTS.err, null],
    [SLOTS.creating, false], [SLOTS.confirm, null],
  ];
  assert.equal(JSON.stringify(r.sets), JSON.stringify(expected), 'the kind-change reset writes every per-kind slot, including creating and confirm');
});

test('replicaCount takes only a whole number from 0 to the int32 limit', async () => {
  const h = await resourceListHarness(reactStub());
  const { replicaCount } = await h.module('ui/resource-list.jsx');
  for (const [v, want] of [
    ['3', 3], [' 3 ', 3], ['0', 0], [0, 0], [5, 5], ['2147483647', 2147483647],
    ['', null], ['  ', null], ['abc', null], ['-1', null], [-1, null], ['1.5', null], [1.5, null],
    ['1e3', null], ['1e999', null], [Infinity, null], [NaN, null], ['2147483648', null],
    [null, null], [undefined, null], ['0x10', null],
  ]) {
    assert.equal(replicaCount(v), want, `replicaCount(${JSON.stringify(v)})`);
  }
});

test('Scale refuses input that is not a replica count instead of patching 0 replicas', async () => {
  const patches = [], flashes = [];
  const r = reactStub();
  const h = await harness({ stubs: {
    react: r.react,
    './shell.jsx': { Modal: 'Modal', PaneShell: 'PaneShell' },
    './codeview.jsx': { CodeView: 'CodeView', CodeEditor: 'CodeEditor' },
    '/k8sutil.js': { k8sGet: () => {}, k8sDelete: () => {}, k8sPost: () => {}, k8sPut: () => {},
      k8sPatch: async (path, body) => { patches.push([path, body]); return { ok: true }; },
      listItems: () => [], nameOf: (o) => o?.metadata?.name || '', nsOf: () => '', meta: (o) => o?.metadata || {},
      age: () => '', labelPairs: () => [] },
    '/resources.jsx': { resourceSearch: () => '', instancePath: () => '/apis/apps/v1/namespaces/a/deployments/web', createPath: () => '' },
  } });
  const { ResourceList } = await h.module('ui/resource-list.jsx');
  const pane = () => {
    r.reset();
    r.state[SLOTS.sel] = { metadata: { namespace: 'a', name: 'web', uid: 'u' }, spec: { replicas: 3 } };
    const [p] = collect(ResourceList({ descriptor, namespace: 'all-namespaces', onFlash: (m) => flashes.push(m) }),
      (n) => n.type?.name === 'DetailPane');
    return p;
  };

  for (const bad of ['', 'abc', '-1', '1.5', '1e999', NaN]) {
    await pane().props.onScale(bad);
  }
  assert.deepEqual(patches, [], 'no PATCH for any invalid input');
  assert.equal(flashes.length, 6);
  assert.ok(flashes.every((m) => m.startsWith('Error:') && !m.includes('Scaled')), 'each refusal flashes an error, never success');

  await pane().props.onScale(0);
  await pane().props.onScale('2');
  assert.equal(patches.length, 2);
  assert.equal(patches[0][0], '/apis/apps/v1/namespaces/a/deployments/web/scale');
  assert.equal(patches[0][1].spec.replicas, 0, 'an explicit 0 still scales to zero');
  assert.equal(patches[1][1].spec.replicas, 2);
});
