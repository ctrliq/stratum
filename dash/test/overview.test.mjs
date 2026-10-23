import assert from 'node:assert/strict';
import test from 'node:test';
import { harness, text, collect } from './harness.mjs';

const shell = {
  StatusPill: 'StatusPill', Meter: 'Meter', PaneShell: 'PaneShell', Sparkline: 'Sparkline',
  nodeTint: () => '', clusterContext: () => 'ctx', clusterProvider: () => 'provider',
};

test('the Nodes Ready tile never claims health it has not read', async () => {
  const h = await harness({ stubs: { '/shell.jsx': shell } });
  const Overview = await h.component('screens/overview.jsx', 'Overview');
  const render = (props) => Overview({ data: { NODES: [], ...props.data }, cluster: props.cluster });
  const tile = (props) => text(render(props));
  // text() walks strings only, so the counts are gathered off the stat-value node.
  const nums = (t) => Array.isArray(t) ? t.flatMap(nums)
    : typeof t === 'number' ? [t]
    : t && typeof t === 'object' ? nums(t.props && t.props.children) : [];
  const counts = (props) => nums(collect(render(props), (n) => n.props && n.props.className === 'stat-value')[0]);

  // Nothing loaded: the cluster has no verdict yet, and neither does the tile.
  const cold = tile({ cluster: null, data: {} });
  assert.match(cold, /Not loaded yet/);
  assert.doesNotMatch(cold, /All healthy/);

  // A cluster that answered with no nodes is a fact, not a pending state.
  const empty = tile({ cluster: { name: 'local', status: 'degraded', nodeCount: 0, readyNodes: 0 }, data: {} });
  assert.match(empty, /No nodes/);
  assert.doesNotMatch(empty, /All healthy|Not loaded yet/);

  // The rows depend on the pod list; the identity does not. A cluster with
  // nodes whose rows could not be built is not a cluster with no nodes.
  const rowless = tile({ cluster: { name: 'local', status: 'healthy', nodeCount: 3, readyNodes: 3 }, data: {} });
  assert.match(rowless, /node details unavailable/);
  assert.doesNotMatch(rowless, /No nodes|All healthy/);
  assert.deepEqual(counts({ cluster: { name: 'local', status: 'healthy', nodeCount: 3, readyNodes: 3 }, data: {} }),
    [3, 3], 'the count the cluster identity does carry is still shown');

  // Stale rows must not outvote the identity: /pods failing after a node left
  // keeps the old rows, while K.CLUSTER is rebuilt from the node list that did
  // answer. The tile follows the identity.
  const stale = tile({ cluster: { name: 'local', status: 'degraded', nodeCount: 2, readyNodes: 1 },
    data: { NODES: [{ name: 'a', ready: true }, { name: 'b', ready: true }, { name: 'gone', ready: true }] } });
  assert.match(stale, /1 not ready/);
  assert.doesNotMatch(stale, /All healthy/);

  // And a cordoned-but-ready node counts as ready.
  const live = tile({ cluster: { name: 'local', status: 'healthy', nodeCount: 2, readyNodes: 2 },
    data: { NODES: [{ name: 'a', ready: true }, { name: 'b', ready: true, cordoned: true }] } });
  assert.match(live, /All healthy/);
  const down = tile({ cluster: { name: 'local', status: 'degraded', nodeCount: 2, readyNodes: 1 },
    data: { NODES: [{ name: 'a', ready: true }, { name: 'b', ready: false }] } });
  assert.match(down, /1 not ready/);
});
