// The sidebar's selected row has to read as selected. The stock dark tokens
// draw it as the --quantic-bg-secondary fill on a --quantic-bg-primary
// sidebar; the Stratum re-skin in app.css darkens both, and left on
// bg-secondary the row sat four RGB points off the canvas. This pins the row
// to at least the contrast the stock tokens give.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';

const read = (f) => fs.readFileSync(new URL(`../src/${f}`, import.meta.url), 'utf8');
const rootTokens = (css) => {
  const block = css.match(/:root\s*\{([^}]*)\}/)[1];
  return Object.fromEntries([...block.matchAll(/(--[\w-]+)\s*:\s*([^;]+);/g)].map((m) => [m[1], m[2].trim()]));
};
const tokens = { ...rootTokens(read('theme.css')), ...rootTokens(read('app.css')) };

function resolve(expr) {
  const m = expr.match(/^var\((--[\w-]+)(?:\s*,\s*(.+))?\)$/);
  if (!m) return expr;
  return tokens[m[1]] !== undefined ? resolve(tokens[m[1]]) : resolve(m[2]);
}
function luminance(hex) {
  const [r, g, b] = hex.replace('#', '').match(/../g).map((h) => parseInt(h, 16) / 255)
    .map((c) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4));
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}
const contrast = (a, b) => {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
};

// Stock dark tokens: --quantic-bg-secondary on --quantic-bg-primary.
const STOCK = contrast('#1a1e25', '#13161b');

test('the selected sidebar row stands out from the sidebar at least as much as with the stock tokens', () => {
  const rule = read('ui/controls.css').match(/\.qc-nav-item\.is-selected\s*\{\s*background-color:\s*([^;]+);/);
  assert.ok(rule, 'the selected nav row sets a background');
  const selected = resolve(rule[1].trim());
  const canvas = resolve('var(--quantic-bg-primary)');
  const got = contrast(selected, canvas);
  assert.ok(got >= STOCK, `selected ${selected} on ${canvas}: contrast ${got.toFixed(3)} < the stock tokens' ${STOCK.toFixed(3)}`);
});

test('a hovered row stays distinguishable from the selected one', () => {
  const hover = resolve('var(--quantic-bg-secondary-hover)');
  const rule = read('ui/controls.css').match(/\.qc-nav-item\.is-selected\s*\{\s*background-color:\s*([^;]+);/);
  assert.notEqual(resolve(rule[1].trim()), hover);
});
