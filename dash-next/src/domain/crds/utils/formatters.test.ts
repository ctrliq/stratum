import { describe, expect, it } from 'vitest';

import type { CustomResourceDefinition } from '~/api/k8s/k8sSchemas';

import {
  toApiPath,
  toDefinitionName,
  toDefinitionsHref,
  toGuessedKind,
  toInstancesHref,
  toInstanceTags,
  toResolvingText,
  toUnreadableNote,
  toUnresolvedTitle,
} from './formatters';

const address = { group: 'example.io', version: 'v1', plural: 'widgets' };

const toDefinition = (scope: string, shortNames?: string[]): CustomResourceDefinition => ({
  spec: {
    group: 'example.io',
    scope,
    names: { kind: 'Widget', plural: 'widgets', shortNames },
    versions: [],
  },
});

describe('hrefs', () => {
  it('address instances under the cluster by group, version and plural', () => {
    expect([
      toInstancesHref('local', address, undefined),
      toInstancesHref('edge 7', address, 'payments'),
    ]).toEqual([
      '/c/local/crds/example.io/v1/widgets',
      '/c/edge%207/crds/example.io/v1/widgets?ns=payments',
    ]);
  });

  it('return to the definitions keeping the namespace', () => {
    expect([toDefinitionsHref('local', 'payments'), toDefinitionsHref('local', undefined)]).toEqual(
      ['/c/local/crds?ns=payments', '/c/local/crds'],
    );
  });
});

describe('texts', () => {
  it('name the definition being resolved', () => {
    expect([toDefinitionName(address), toResolvingText(address)]).toEqual([
      'widgets.example.io',
      'Resolving widgets.example.io…',
    ]);
  });

  it('name the API path listed when the definition is unreadable', () => {
    expect([toApiPath(address), toUnreadableNote(address), toUnresolvedTitle(address)]).toEqual([
      '/apis/example.io/v1/widgets',
      'definition not readable — listing /apis/example.io/v1/widgets',
      'widgets (example.io)',
    ]);
  });
});

describe('toGuessedKind', () => {
  it.each([
    ['widgets', 'Widget'],
    ['policies', 'Policy'],
    ['gateways', 'Gateway'],
    ['meshes', 'Meshe'],
    ['status', 'Statu'],
    ['sheep', 'Sheep'],
  ])('guesses %s as %s', (plural, kind) => {
    expect(toGuessedKind(plural)).toBe(kind);
  });
});

describe('toInstanceTags', () => {
  it.each([
    ['Namespaced', undefined, ['example.io/v1', 'Namespaced']],
    ['Cluster', undefined, ['example.io/v1', 'Cluster-scoped']],
    ['Namespaced', ['wd', 'wdg'], ['example.io/v1', 'Namespaced', 'short: wd, wdg']],
  ])('tags a %s kind with short names %o as %o', (scope, shortNames, tags) => {
    expect(toInstanceTags(toDefinition(scope, shortNames), address)).toEqual(tags);
  });

  it('shows only the API version without a definition', () => {
    expect(toInstanceTags(undefined, address)).toEqual(['example.io/v1']);
  });
});
