// The YAML view's toYaml() must render empty maps and lists the way
// kubectl -o yaml does: an empty object is `{}` and an empty list is `[]`.
// On base an empty object falls through to yamlScalar, whose String({})
// is "[object Object]" (JSON-quoted by the quoting rules), and an empty
// list becomes String([]) === '' and renders as `''`. Pods whose
// containers set no resources hit this on every Object -> YAML view.
//
// codeview.jsx is JSX, so the module runs in its own vm like
// signout.test.mjs does: the oxc transform strips the JSX and the two
// imports (react, controls.jsx) are stubbed — toYaml itself runs for real.
import assert from 'node:assert/strict';
import test from 'node:test';
import fs from 'node:fs/promises';
import vm from 'node:vm';
import { transformWithOxc } from 'vite';

const loadToYaml = async () => {
  let code = await fs.readFile(new URL('../src/ui/codeview.jsx', import.meta.url), 'utf8');
  ({ code } = await transformWithOxc(code, 'codeview.jsx', { jsx: { runtime: 'classic', pragma: '__jsx', pragmaFrag: '__fragment' } }));
  const context = vm.createContext({
    __jsx: (type, props, ...children) => ({ type, props: { ...props, children } }),
    __fragment: 'fragment',
  });
  const mod = new vm.SourceTextModule(code, { context, identifier: 'codeview.jsx' });
  const stub = (values) => new vm.SyntheticModule(Object.keys(values), function () {
    for (const [name, value] of Object.entries(values)) this.setExport(name, value);
  }, { context });
  await mod.link(async (specifier) => {
    if (specifier === 'react') return stub({
      useState: (v) => [v, () => {}], useMemo: (fn) => fn(), useRef: () => ({ current: null }),
      useLayoutEffect: () => {}, Fragment: 'fragment',
    });
    if (specifier.endsWith('controls.jsx')) return stub({ Button: 'Button' });
    throw new Error('Unmocked dependency in the codeview loader: ' + specifier);
  });
  await mod.evaluate();
  return mod.namespace.toYaml;
};
const toYaml = await loadToYaml();

test('an empty object value renders as {} where kubectl shows {}', () => {
  assert.equal(toYaml({ resources: {} }), 'resources: {}');
});

test('an empty array value renders as [] not the empty scalar', () => {
  assert.equal(toYaml({ args: [] }), 'args: []');
});

test('empty objects and arrays render as array items too', () => {
  assert.equal(toYaml({ items: [{}, []] }), 'items:\n  - {}\n  - []');
});

test('a pod whose container sets no resources renders resources: {} at depth', () => {
  const pod = {
    apiVersion: 'v1',
    kind: 'Pod',
    metadata: { name: 'p', namespace: 'default' },
    spec: {
      containers: [{ name: 'app', image: 'busybox', resources: {} }],
    },
  };
  assert.equal(toYaml(pod), [
    'apiVersion: v1',
    'kind: Pod',
    'metadata:',
    '  name: p',
    '  namespace: default',
    'spec:',
    '  containers:',
    '    - name: app',
    '      image: busybox',
    '      resources: {}',
  ].join('\n'));
});

test('non-empty maps and lists keep their existing output', () => {
  assert.equal(toYaml({ resources: { requests: { cpu: '250m' }, limits: { memory: '512Mi' } } }),
    'resources:\n  requests:\n    cpu: "250m"\n  limits:\n    memory: "512Mi"');
  assert.equal(toYaml({ args: ['--verbose', 'x'] }), 'args:\n  - "--verbose"\n  - x');
  assert.equal(toYaml({ x: null, y: 0, z: 'plain' }), 'x: null\ny: 0\nz: plain');
});
