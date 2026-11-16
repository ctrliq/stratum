import { describe, expect, it } from 'vitest';

import { unresolvedConfig } from '~/shared/context/ConfigContext';

import { fromColumnPath } from './parsers';
import { hasUnresolvedNamespace, isHubManaged, isIdentityPath, isShownColumn } from './predicates';
import { toInstanceKind } from './transforms';

const hub = { ...unresolvedConfig, auth: { required: true, providers: ['local'] } };
const dashboardServer = { ...unresolvedConfig, auth: { required: true, providers: ['token'] } };

describe('isIdentityPath', () => {
  it('recognises name, namespace and creation time only', () => {
    expect(
      [
        '.metadata.name',
        '$.metadata.namespace',
        '{.metadata.creationTimestamp}',
        '.metadata.uid',
        '.a[0',
      ].map((path) => isIdentityPath(fromColumnPath(path))),
    ).toEqual([true, true, true, false, false]);
  });
});

describe('isShownColumn', () => {
  it('needs a name, a path and no priority above 0', () => {
    expect(
      [
        { name: 'A', jsonPath: '.a' },
        { name: 'A', jsonPath: '.a', priority: 0 },
        { name: 'A', jsonPath: '.a', priority: 1 },
        { name: '', jsonPath: '.a' },
        { name: 'A', jsonPath: '' },
      ].map(isShownColumn),
    ).toEqual([true, true, false, false, false]);
  });
});

describe('isHubManaged', () => {
  it.each([
    ['example.io', 'widgets', false],
    ['stratum.ciq.com', 'helmcharts', true],
    ['stratum.ciq.com', 'helmchartconfigs', true],
    ['example.io', 'helmcharts', false],
  ])('on the hub, %s %s is read-only: %s', (group, plural, isReadOnly) => {
    expect(isHubManaged(hub, { group, plural })).toBe(isReadOnly);
  });

  it('is never read-only on the dashboard server', () => {
    expect(isHubManaged(dashboardServer, { group: 'stratum.ciq.com', plural: 'helmcharts' })).toBe(
      false,
    );
  });
});

describe('hasUnresolvedNamespace', () => {
  const address = { group: 'example.com', version: 'v1', plural: 'widgets' };
  const definition = {
    metadata: { name: 'widgets.example.com' },
    spec: {
      group: 'example.com',
      names: { kind: 'Widget', plural: 'widgets' },
      scope: 'Namespaced',
      versions: [],
    },
  };

  it('gives the list its own Namespace column when unread instances carry a namespace', () => {
    const kind = toInstanceKind(undefined, address, [
      { metadata: { name: 'a', namespace: 'shop' } },
    ]);
    expect(hasUnresolvedNamespace(undefined, kind)).toBe(true);
  });

  it('leaves Name as the only column when no listed instance carries a namespace', () => {
    const kind = toInstanceKind(undefined, address, [{ metadata: { name: 'a' } }]);
    expect(hasUnresolvedNamespace(undefined, kind)).toBe(false);
  });

  it('leaves the column to the identity columns once the definition is read', () => {
    const kind = toInstanceKind(definition, address, []);
    expect(hasUnresolvedNamespace(definition, kind)).toBe(false);
  });
});
