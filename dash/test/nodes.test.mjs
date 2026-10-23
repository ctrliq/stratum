import assert from 'node:assert/strict';
import test from 'node:test';
import { harness, collect, nodes, text } from './harness.mjs';

// The cordon control PATCHes spec.unschedulable and nothing else — no
// eviction subresource is called anywhere in dash/. A label promising a drain
// makes the UI lie about what the click did, and an operator who believes it
// reboots a node whose pods never moved. Guard the label, not the handler:
// whoever implements a real drain has to flip this test
// deliberately.
test('node controls promise a cordon and never a drain', async () => {
  const h = await harness({
    config: { mode: 'standalone', auth: { required: true } },
    // The harness has no roles; a signed-in session is what useCanWrite gates on.
    signedIn: true,
  });
  await h.resolveConfig(); await h.signIn();
  // `ready` is the readiness flag split out of `status`; both are set so
  // the fixture is a node the screens would actually render.
  const node = { name: 'worker', status: 'Ready', ready: true, cordoned: false };
  // The row icon button lives in Nodes, the footer button in its detail pane.
  const Nodes = await h.component('screens/infra.jsx', 'Nodes');
  const NodeDetail = await h.component('screens/infra.jsx', 'NodeDetail');
  const trees = [
    Nodes({ data: { NODES: [node] }, onAction: () => {} }),
    NodeDetail({ node, data: {}, onAction: () => {} }),
  ];
  const buttons = trees.flatMap((tree) => nodes(tree, 'Button'));
  const row = buttons.filter((b) => /cordon/i.test(b.props.title || ''));
  const footer = buttons.filter((b) => /cordon/i.test(text(b)));
  assert.equal(row.length, 1, 'the row icon button is present');
  assert.equal(footer.length, 1, 'the detail footer button is present');
  assert.equal(row[0].props.title, 'Cordon');
  assert.equal(text(footer[0]), 'Cordon');
  // Backstop for a third control: no serializable label may claim a drain.
  for (const tree of trees) assert.equal(/drain/i.test(JSON.stringify(tree)), false);
});

// `stratum.ciq.com/k8s-version` carries the UPSTREAM Kubernetes
// version; the kubelet carries the SKE build OF it, with a `-stratum` suffix
// that tools/inttest/check-single-node.sh asserts on every CI run. The two
// must not be compared raw, or every healthy node reads as drifted — and an
// indicator that is always on hides the partial upgrade it exists to catch.
// Both the row badge and the header count go through the one shared
// comparison, and both are asserted here.
test('version drift compares Kubernetes versions, not the -stratum build string', async () => {
  const h = await harness({ config: { mode: 'standalone', auth: { required: true } }, signedIn: true });
  await h.resolveConfig(); await h.signIn();
  const Nodes = await h.component('screens/infra.jsx', 'Nodes');
  const row = (name, version, stratumVersion) =>
    ({ name, status: 'Ready', ready: true, cordoned: false, version, stratumVersion });
  // The row's version cell is a component the inert harness does not descend
  // into, so call it with the props the row handed it. Assert it was found:
  // "no drift badge" is otherwise what a cell that stopped matching also says.
  const badges = (tree) => {
    const cells = collect(tree, (n) => typeof n.type === 'function' && n.type.name === 'VersionCell');
    assert.equal(cells.length, 1, 'the row still renders its version cell');
    return cells.flatMap((cell) => nodes(cell.type(cell.props), 'Badge')).filter((b) => /drift/i.test(text(b)));
  };

  const aligned = Nodes({ data: { NODES: [row('a', 'v1.37.0-stratum', 'v1.37.0')] } });
  assert.deepEqual(badges(aligned), [], 'the SKE build of the stamped version is not drift');
  assert.equal(/version drift/.test(text(aligned)), false, 'and the header counts none');

  const skewed = Nodes({ data: { NODES: [row('b', 'v1.36.4-stratum', 'v1.37.0')] } });
  assert.equal(badges(skewed).length, 1, 'a different upstream version still drifts');
  assert.match(text(skewed), /1 version drift/);
});

// `status.nodeInfo.kubeletVersion` is already `v`-prefixed, so the
// detail pane's literal `'v' +` rendered `vv1.37.0-stratum` — and `v—` on a
// node that reports no kubelet at all. The list view on the same screen renders
// the identical value straight through, so the two disagreed about one string.
// Assert the whole System info block passes its values through untouched, not
// just the one row: that is the property the slip broke.
test('node detail renders versions exactly as the node reports them', async () => {
  const h = await harness({ config: { mode: 'standalone', auth: { required: true } }, signedIn: true });
  await h.resolveConfig(); await h.signIn();
  const NodeDetail = await h.component('screens/infra.jsx', 'NodeDetail');
  // Every System info row, keyed by its label, with a distinct value each so a
  // row rendering its neighbour's value is caught too.
  const info = {
    'Instance type': '4', Zone: 'us-east-1a', Kubelet: 'v1.37.0-stratum',
    'OS image': 'Alpine Linux v3.24', 'Stratum k8s-version': 'v1.37.0',
    'Container runtime': 'containerd://2.3.5+stratum', Kernel: '6.12.0',
  };
  const node = (over = {}) => ({
    name: 'worker', status: 'Ready', ready: true, cordoned: false,
    instance: info['Instance type'], zone: info.Zone, version: info.Kubelet,
    os: info['OS image'], stratumVersion: info['Stratum k8s-version'],
    runtime: info['Container runtime'], kernel: info.Kernel, ...over,
  });
  const rowsOf = (n) => Object.fromEntries(
    collect(NodeDetail({ node: n, data: {}, onAction: () => {} }),
            (x) => typeof x.type === 'function' && x.type.name === 'KvP')
      .map((x) => [x.props.k, x.props.v]));

  // The whole block, not just Kubelet: the property the slip broke is that
  // System info passes its values through untouched, and pinning one row
  // leaves the next `'v' +` on the neighbouring line unguarded. Assert the row
  // set as well, or a row that stops rendering reads as a row that passes.
  const rows = rowsOf(node());
  assert.deepEqual(rows, info);

  // live.js defaults each of these to '—' when the node does not report it
  // (dash/src/live.js), so a decorated placeholder renders as 'v—'. Blanking
  // them all keeps the check on the block rather than on the kubelet row.
  const blanked = Object.fromEntries(Object.keys(info).map((k) => [k, '—']));
  assert.deepEqual(
    rowsOf(node({ version: '—', stratumVersion: '—', runtime: '—', kernel: '—', instance: '—', zone: '—', os: '—' })),
    blanked, 'an unreported value renders the placeholder alone, never "v—"');
});
