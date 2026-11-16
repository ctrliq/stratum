import { describe, expect, it } from 'vitest';

import { dashboardConfig, hubConfig } from '~/domain/auth/testing/fixtures';

import { edgeCluster, emptyFleet, fleetOfTwo, hubCluster, read } from '../testing/fixtures';
import {
  clusterIn,
  clustersIn,
  clustersOf,
  countOf,
  defaultClusterIdOf,
  linkOf,
  listedNamesOf,
  namesOf,
  pathOf,
  refreshedAtOf,
  screenAt,
  selectedRowOf,
  staleListsOf,
  totalOf,
} from './selectors';

describe('defaultClusterIdOf', () => {
  it('is "local" on a single-cluster backend', () => {
    expect(defaultClusterIdOf({ status: 'single' })).toBe('local');
  });

  it('is the fleet manager, else the first cluster', () => {
    expect([
      defaultClusterIdOf(fleetOfTwo),
      defaultClusterIdOf({ status: 'ready', clusters: [edgeCluster] }),
    ]).toEqual(['hub', 'edge']);
  });

  it('is nothing for an empty, loading or unreadable fleet', () => {
    expect(
      [emptyFleet, { status: 'loading' } as const, { status: 'unreadable' } as const].map(
        defaultClusterIdOf,
      ),
    ).toEqual([undefined, undefined, undefined]);
  });
});

describe('clusterIn and clustersOf', () => {
  it('find clusters only in a fleet that was read', () => {
    expect([
      clusterIn(fleetOfTwo, 'edge'),
      clusterIn(fleetOfTwo, 'gone'),
      clusterIn({ status: 'single' }, 'local'),
      clustersOf({ status: 'unreadable' }),
    ]).toEqual([edgeCluster, undefined, undefined, []]);
    expect(clustersOf(fleetOfTwo)).toEqual([edgeCluster, hubCluster]);
  });
});

describe('clustersIn', () => {
  it('tolerates a cluster list without items', () => {
    expect([clustersIn({}), clustersIn({ items: [hubCluster] })]).toEqual([[], [hubCluster]]);
  });
});

describe('screenAt', () => {
  it('finds a cluster screen by its first segment', () => {
    expect(screenAt({ clusterId: 'a', segments: ['pods'] })?.label).toBe('Pods');
  });

  it('finds a fleet screen only outside a cluster', () => {
    expect([
      screenAt({ segments: ['clusters'] })?.id,
      screenAt({ clusterId: 'a', segments: ['clusters'] }),
      screenAt({ clusterId: 'a', segments: ['nope'] }),
    ]).toEqual(['clusters', undefined, undefined]);
  });
});

describe('selectedRowOf', () => {
  it('is the screen itself', () => {
    expect(selectedRowOf({ clusterId: 'a', segments: ['pods'] }, undefined)).toBe('pods');
  });

  it('is the tab an old tabbed screen shows, the first tab by default', () => {
    expect([
      selectedRowOf({ clusterId: 'a', segments: ['storage'] }, 'storageclasses'),
      selectedRowOf({ clusterId: 'a', segments: ['storage'] }, 'bogus'),
      selectedRowOf({ clusterId: 'a', segments: ['rbac'] }, undefined),
    ]).toEqual(['storageclasses', 'persistentvolumeclaims', 'roles']);
  });

  it('is the Ingresses row for both ingress links', () => {
    expect(
      ['ingress', 'ingresses'].map((segment) =>
        selectedRowOf({ clusterId: 'a', segments: [segment] }, undefined),
      ),
    ).toEqual(['ingress', 'ingress']);
  });

  it('is the same custom resource row at any version', () => {
    expect(
      ['v1', 'v1beta1'].map((version) =>
        selectedRowOf(
          { clusterId: 'a', segments: ['crds', 'example.io', version, 'widgets'] },
          undefined,
        ),
      ),
    ).toEqual(['crds/example.io/widgets', 'crds/example.io/widgets']);
  });
});

describe('linkOf', () => {
  it('is the configured URL, or nothing when the link is absent or empty', () => {
    expect([
      linkOf(hubConfig, 'grafana'),
      linkOf(dashboardConfig, 'pro'),
      linkOf(dashboardConfig, 'grafana'),
    ]).toEqual(['https://grafana.example', undefined, undefined]);
  });
});

describe('staleListsOf', () => {
  it('names each list that kept old data, once', () => {
    expect(
      staleListsOf([
        read({ failureCount: 1 }),
        read({ failureCount: 2 }),
        read({ name: 'nodes' }),
        read({ name: 'events', failureCount: 1, hasData: false }),
      ]),
    ).toEqual(['pods']);
  });
});

describe('refreshedAtOf', () => {
  it('is the latest answer, zero before any', () => {
    expect([
      refreshedAtOf([read({ answeredAt: 5 }), read({ answeredAt: 9 })]),
      refreshedAtOf([]),
    ]).toEqual([9, 0]);
  });
});

describe('namesOf and countOf', () => {
  const list = {
    items: [{ metadata: { name: 'kube-system' } }, { metadata: { name: 'default' } }, {}],
  };

  it('list names in order and count items', () => {
    expect([namesOf(list), countOf(list)]).toEqual([['default', 'kube-system'], 3]);
  });
});

describe('totalOf', () => {
  it('adds the counts that were read and is unknown until one was', () => {
    expect([totalOf([2, undefined, 3]), totalOf([undefined, undefined]), totalOf([0])]).toEqual([
      5,
      undefined,
      0,
    ]);
  });
});

describe('listedNamesOf and pathOf', () => {
  it('keeps names in the order the cluster listed them', () => {
    expect(
      listedNamesOf({
        items: [{ metadata: { name: 'zeta' } }, {}, { metadata: { name: 'alpha' } }],
      }),
    ).toEqual(['zeta', 'alpha']);
  });

  it('reads the request path out of a query key', () => {
    expect([
      pathOf(['k8s', 'edge', 'apis', 'batch', 'v1', 'jobs', { limit: 5 }]),
      pathOf(['api', 'v1', 'clusters']),
    ]).toEqual(['/apis/batch/v1/jobs', '/api/v1/clusters']);
  });
});
