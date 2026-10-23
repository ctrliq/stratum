import assert from 'node:assert/strict';
import test from 'node:test';
import { parseSplit, layoutOf, constrainPair, paneBounds } from '../src/ui/split.js';

const round = (xs) => xs.map((x) => Math.round(x * 10) / 10);

test('a declared split is the split that renders', () => {
  // The value the resource list and the nodes screen actually pass.
  assert.deepEqual(round(parseSplit('64% 36%', 2)), [64, 36]);
  // Why that literal has spaces AND percent signs: "64,36" is ONE unitless
  // token, i.e. a flex weight of 64 against the second pane's default 1, which
  // leaves the detail pane a 1.5% sliver. The parser is faithful to the design
  // system here, so the call sites must not write it that way.
  assert.deepEqual(round(parseSplit('64,36', 2)), [98.5, 1.5]);
  // Mixed: the percentage is claimed first, the rest goes to the flex slots.
  assert.deepEqual(round(parseSplit('30% *', 2)), [30, 70]);
  assert.deepEqual(round(parseSplit('50% 1 1', 3)), [50, 25, 25]);
  // No declaration, a short one, or nonsense: equal shares of what is left.
  assert.deepEqual(round(parseSplit('', 2)), [50, 50]);
  assert.deepEqual(round(parseSplit('60%', 2)), [60, 40]);
  assert.deepEqual(round(parseSplit('oops', 2)), [50, 50]);
  // Over-subscribed, and the degenerate cases.
  assert.deepEqual(round(parseSplit('80% 80%', 2)), [50, 50]);
  assert.deepEqual(parseSplit('64% 36%', 0), []);
});

test('a collapsed pane gives its share to the others and gets it back', () => {
  const sizes = parseSplit('64% 36%', 2);
  const open = [{ collapsed: false }, { collapsed: false }];
  const shut = [{ collapsed: false }, { collapsed: true }];
  assert.deepEqual(round(layoutOf(sizes, open)), [64, 36]);
  assert.deepEqual(round(layoutOf(sizes, shut)), [100, 0]);
  // `sizes` is untouched, so reopening restores the declared split.
  assert.deepEqual(round(layoutOf(sizes, open)), [64, 36]);
  assert.deepEqual(layoutOf([50, 50], [{ collapsed: true }, { collapsed: true }]), [0, 0]);
});

test('dragging a divider stays inside both panes bounds and keeps their total', () => {
  const bounds = { a: paneBounds({ minSize: 30 }), b: paneBounds({ minSize: 26, maxSize: 66 }), total: 100 };
  assert.deepEqual(constrainPair(64, 0, bounds), [64, 36]);
  assert.deepEqual(constrainPair(64, 5, bounds), [69, 31]);
  // Dragged past the left pane's min-size, and past the right pane's.
  assert.deepEqual(constrainPair(64, -60, bounds), [34, 66]);
  assert.deepEqual(constrainPair(64, 60, bounds), [74, 26]);
  for (const delta of [-100, -1, 0, 1, 100]) {
    const [a, b] = constrainPair(64, delta, bounds);
    assert.equal(Math.round(a + b), 100, 'the pair keeps its combined share');
  }
  // min/max the wrong way round must not invert the pane.
  assert.deepEqual(paneBounds({ minSize: 80, maxSize: 20 }), { min: 20, max: 80 });
});
