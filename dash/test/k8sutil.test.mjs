import assert from 'node:assert/strict';
import test from 'node:test';

// Regression guard: k8sutil.js's helpers are package exports
// (package.json exports every file under src), so k8sutil.js must keep
// re-exporting the shared rules with unchanged values — a consumer may
// already import them from here.

const k8sutil = await import('../src/k8sutil.js');

test('the quantity and age helpers k8sutil.js exports keep their values', async (t) => {
  // The export set itself is pinned: dropping a re-export must fail
  // here, not in a downstream package build.
  assert.deepEqual(Object.keys(k8sutil).sort(),
    ['age', 'cpuToM', 'k8sDelete', 'k8sFetch', 'k8sGet', 'k8sPatch', 'k8sPost',
      'k8sPut', 'labelPairs', 'listItems', 'memToMi', 'meta', 'nameOf', 'nsOf']);
  for (const name of Object.keys(k8sutil)) assert.equal(typeof k8sutil[name], 'function', name);

  // Kubernetes quantity → millicores.
  for (const [q, millicores] of [
    ['250m', 250], ['2', 2000], [0.5, 500], ['1500000n', 1.5], ['500u', 0.5], [null, 0],
  ]) {
    assert.equal(k8sutil.cpuToM(q), millicores, `cpuToM(${String(q)})`);
  }

  // Kubernetes quantity → MiB. 1M is a decimal megabyte over 1048576 bytes.
  for (const [q, mib] of [
    ['512Mi', 512], ['1Gi', 1024], ['1024Ki', 1], [null, 0], ['not-a-quantity', 0],
  ]) {
    assert.equal(k8sutil.memToMi(q), mib, `memToMi(${String(q)})`);
  }
  assert.equal(Number(k8sutil.memToMi('1M').toFixed(6)), 0.953674, 'memToMi(1M)');

  // Relative age. The clock is mocked so the buckets cannot drift under the run.
  t.mock.timers.enable({ apis: ['Date'] });
  t.mock.timers.setTime(1700000000000);
  const ts = (secondsAgo) => new Date(1700000000000 - secondsAgo * 1000).toISOString();
  assert.equal(k8sutil.age(null), '—', 'age of no timestamp');
  assert.equal(k8sutil.age(undefined), '—', 'age of no timestamp');
  assert.equal(k8sutil.age(ts(30)), '30s', 'age of a 30s-old timestamp');
  assert.equal(k8sutil.age(ts(3 * 3600)), '3h', 'age of a 3h-old timestamp');
  assert.equal(k8sutil.age(ts(3 * 86400)), '3d', 'age of a 3d-old timestamp');
  t.mock.timers.reset();
});
