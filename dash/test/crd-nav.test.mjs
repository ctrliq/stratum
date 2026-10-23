import assert from 'node:assert/strict';
import test from 'node:test';
import { crdNavGroups, crdRoute, sameCrdRoute, servedVersion } from '../src/crd-nav.js';

const crd = (group, plural, kind, versions, scope = 'Namespaced') => ({ spec: { group, scope, names: { plural, kind }, versions } });

test('one sub-group per API group, groups and kinds in order', () => {
  const got = crdNavGroups([
    crd('z.example.com', 'widgets', 'Widget', [{ name: 'v1', served: true }]),
    crd('a.example.com', 'things', 'Thing', [{ name: 'v1', served: true }], 'Cluster'),
    crd('a.example.com', 'gadgets', 'Gadget', [{ name: 'v1alpha1', served: false }, { name: 'v1beta1', served: true }]),
  ]);
  assert.deepEqual(got, [
    { group: 'a.example.com', items: [
      { id: 'crds/a.example.com/v1beta1/gadgets', label: 'Gadget' },
      { id: 'crds/a.example.com/v1/things', label: 'Thing' },
    ] },
    { group: 'z.example.com', items: [{ id: 'crds/z.example.com/v1/widgets', label: 'Widget' }] },
  ]);
});

test('a row id is the CRD instance route app.jsx parses', () => {
  const id = crdRoute(crd('a.example.com', 'widgets', 'Widget', [{ name: 'v2', served: true }]).spec);
  const [prefix, group, version, plural] = id.split('/');
  assert.deepEqual({ prefix, group, version, plural }, { prefix: 'crds', group: 'a.example.com', version: 'v2', plural: 'widgets' });
});

test('the served version wins; with none served the first listed is read', () => {
  assert.equal(servedVersion({ versions: [{ name: 'v1', served: false }, { name: 'v2', served: true }] }), 'v2');
  assert.equal(servedVersion({ versions: [{ name: 'v1', served: false }] }), 'v1');
  assert.equal(servedVersion({}), 'v1');
});

test('a definition without a group or plural is skipped, a missing kind falls back to the plural', () => {
  assert.deepEqual(crdNavGroups([{ spec: { names: { plural: 'x' } } }, { spec: { group: 'g' } }, null]), []);
  assert.deepEqual(crdNavGroups([{ spec: { group: 'g.io', names: { plural: 'xs' }, versions: [{ name: 'v1', served: true }] } }]),
    [{ group: 'g.io', items: [{ id: 'crds/g.io/v1/xs', label: 'xs' }] }]);
  assert.deepEqual(crdNavGroups(undefined), []);
});

test('two CRD routes match on group and plural, whatever the version', () => {
  assert.equal(sameCrdRoute('crds/a.example.com/v1/widgets', 'crds/a.example.com/v2/widgets'), true);
  assert.equal(sameCrdRoute('crds/a.example.com/v1/widgets', 'crds/b.example.com/v1/widgets'), false);
  assert.equal(sameCrdRoute('crds/a.example.com/v1/widgets', 'crds/a.example.com/v1/gadgets'), false);
  assert.equal(sameCrdRoute('crds/a.example.com/v1/widgets', 'deployments'), false);
  assert.equal(sameCrdRoute('deployments', 'deployments'), false, 'a non-CRD route is not a CRD row');
  assert.equal(sameCrdRoute(undefined, undefined), false);
});
