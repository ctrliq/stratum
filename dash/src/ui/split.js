// Splitter geometry — the arithmetic behind <SplitterGroup> (ui/controls.jsx).
//
// Kept apart from the component because it is the only part of the
// replacement widgets with real arithmetic in it, and here it can be checked
// without a DOM (test/split.test.mjs). The rules are the design system's, so a
// pane declared "64% 36%" lands where that system specifies:
//   - a "<n>%" token claims that share outright;
//   - "<n>" or "*" is a flex weight, sharing whatever the percentages left;
//   - panes with no token get weight 1;
//   - the result is normalised to 100, so an over-subscribed split still fits.
export const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));

export function parseSplit(split, paneCount) {
  if (paneCount === 0) return [];
  const tokens = String(split || '').trim().split(/\s+/).filter(Boolean);
  const slots = Array.from({ length: paneCount }, (_, i) => {
    const t = tokens[i];
    if (t == null) return { percent: 0, flex: 1 };
    if (t.endsWith('%')) {
      const v = parseFloat(t);
      return { percent: Number.isNaN(v) || v < 0 ? 0 : v, flex: 0 };
    }
    const v = t === '*' ? 1 : parseFloat(t);
    return { percent: 0, flex: Number.isNaN(v) || v <= 0 ? 1 : v };
  });
  const explicit = slots.reduce((s, x) => s + x.percent, 0);
  const flexTotal = slots.reduce((s, x) => s + x.flex, 0);
  const remaining = Math.max(0, 100 - explicit);
  const result = slots.map(({ percent, flex }) =>
    (flex === 0 ? percent : flexTotal > 0 ? (flex / flexTotal) * remaining : 0));
  const sum = result.reduce((a, b) => a + b, 0);
  return sum > 0 ? result.map((v) => (v / sum) * 100) : Array(paneCount).fill(100 / paneCount);
}

// A pane's drag limits, as percentages of the group.
export function paneBounds(pane) {
  const lo = clamp(Number.isFinite(pane.minSize) ? pane.minSize : 10, 0, 100);
  const hi = clamp(Number.isFinite(pane.maxSize) ? pane.maxSize : 90, 0, 100);
  return { min: Math.min(lo, hi), max: Math.max(lo, hi) };
}

// What each pane actually gets: a collapsed pane takes nothing and the rest
// share the full width, so closing the detail pane widens the list to 100%
// without touching the stored sizes — reopening restores the split.
export function layoutOf(sizes, panes) {
  if (sizes.length !== panes.length) return sizes;
  const active = panes.reduce((sum, p, i) => (p.collapsed ? sum : sum + sizes[i]), 0);
  if (active === 0) return sizes.map(() => 0);
  return sizes.map((s, i) => (panes[i].collapsed ? 0 : (s / active) * 100));
}

// Move the divider between two panes by `delta` percent, honouring both panes'
// bounds. The pair always keeps its combined share, so the other panes do not
// move while one divider is dragged.
export function constrainPair(currentA, delta, { a, b, total }) {
  const min = Math.max(a.min, total - b.max);
  const max = Math.min(a.max, total - b.min);
  const newA = clamp(currentA + delta, Math.min(min, max), Math.max(min, max));
  return [newA, total - newA];
}
