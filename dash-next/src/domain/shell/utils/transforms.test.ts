import { describe, expect, it } from 'vitest';

import { dashboardConfig, hubConfig, standaloneHubConfig } from '~/domain/auth/testing/fixtures';

import { screens } from '../constants';
import { edgeCluster, emptyFleet, fleetOfTwo, hubCluster } from '../testing/fixtures';
import type { Screen } from '../types/screen';
import {
  toClusterRead,
  toCrdGroups,
  toCrumbs,
  toFleet,
  toLanding,
  toLocalCluster,
  toPaletteEntries,
  toPaletteGroups,
  toScreenHref,
  toScreenPath,
  toSidebarSections,
  toTrailKind,
  withNamespace,
} from './transforms';

const screenWithId = (id: string): Screen =>
  screens.find((screen) => screen.id === id) ?? screens[0];

const hubPlace = { config: hubConfig, clusterId: 'edge', namespace: 'team-a' };
const dashboardPlace = { config: dashboardConfig, clusterId: 'local', namespace: undefined };

const definitions = {
  items: [
    {
      spec: {
        group: 'example.io',
        scope: 'Namespaced',
        names: { kind: 'Widget', plural: 'widgets' },
        versions: [
          { name: 'v1alpha1', served: false, storage: false },
          { name: 'v1', served: true, storage: true },
        ],
      },
    },
    {
      spec: {
        group: 'acme.dev',
        scope: 'Cluster',
        names: { kind: 'Gadget', plural: 'gadgets' },
        versions: [{ name: 'v1', served: true, storage: true }],
      },
    },
  ],
};

describe('toScreenPath and toScreenHref', () => {
  it('address a cluster screen under its cluster, keeping the namespace filter', () => {
    expect([
      toScreenPath(screenWithId('pods'), 'edge/1'),
      toScreenHref(screenWithId('pods'), hubPlace),
      toScreenHref(screenWithId('pods'), dashboardPlace),
    ]).toEqual(['/c/edge%2F1/pods', '/c/edge/pods?ns=team-a', '/c/local/pods']);
  });

  it('address a fleet screen at the top level with no parameters', () => {
    expect(toScreenHref(screenWithId('clusters'), hubPlace)).toBe('/clusters');
  });
});

describe('withNamespace', () => {
  it('sets the filter and keeps the path and other parameters', () => {
    expect(withNamespace('/c/a/pods', '?name=x%2Fy&ns=old', 'team-a')).toBe(
      '/c/a/pods?name=x%2Fy&ns=team-a',
    );
  });

  it('removes the parameter for all namespaces', () => {
    expect([withNamespace('/c/a/pods', '?ns=old', ''), withNamespace('/c/a/pods', '', '')]).toEqual(
      ['/c/a/pods', '/c/a/pods'],
    );
  });
});

describe('toFleet', () => {
  it('is a single cluster on the dashboard server, whatever the read says', () => {
    expect(toFleet(true, { isPending: true })).toEqual({ status: 'single' });
  });

  it('follows the cluster list read on a hub', () => {
    expect([
      toFleet(false, { isPending: true }),
      toFleet(false, { data: [hubCluster], isPending: false }),
      toFleet(false, { isPending: false }),
    ]).toEqual([
      { status: 'loading' },
      { status: 'ready', clusters: [hubCluster] },
      { status: 'unreadable' },
    ]);
  });

  it('is unreadable once a first load has failed, while it is still retried', () => {
    expect([
      toFleet(false, { isPending: true, failureCount: 1 }),
      toFleet(false, { data: [hubCluster], isPending: false, failureCount: 2 }),
    ]).toEqual([{ status: 'unreadable' }, { status: 'ready', clusters: [hubCluster] }]);
  });
});

describe('toLanding', () => {
  it("sends the root to the default cluster's overview and drops the query", () => {
    expect([
      toLanding({ status: 'single' }, dashboardConfig, '/', '?x=1'),
      toLanding(fleetOfTwo, hubConfig, '/', ''),
    ]).toEqual([{ href: '/c/local/overview' }, { href: '/c/hub/overview' }]);
  });

  it('sends an old cluster-less link to the same screen, query kept', () => {
    expect([
      toLanding({ status: 'single' }, dashboardConfig, '/pods', '?ns=team-a'),
      toLanding(fleetOfTwo, hubConfig, '/crds/example.io/v1/widgets', ''),
    ]).toEqual([
      { href: '/c/local/pods?ns=team-a' },
      { href: '/c/hub/crds/example.io/v1/widgets' },
    ]);
  });

  it('waits for the cluster list', () => {
    expect(toLanding({ status: 'loading' }, hubConfig, '/', '')).toBeUndefined();
  });

  it('leaves a path that is already under a cluster alone', () => {
    expect(toLanding(fleetOfTwo, hubConfig, '/c/hub/overview', '')).toBeUndefined();
  });

  it('shows an empty or unreadable fleet as such, without a stand-in cluster', () => {
    expect([
      toLanding(emptyFleet, hubConfig, '/', ''),
      toLanding(emptyFleet, hubConfig, '/clusters', ''),
      toLanding(emptyFleet, standaloneHubConfig, '/', ''),
      toLanding({ status: 'unreadable' }, hubConfig, '/', ''),
      toLanding({ status: 'unreadable' }, hubConfig, '/clusters', ''),
      toLanding({ status: 'unreadable' }, standaloneHubConfig, '/', ''),
    ]).toEqual([
      { href: '/clusters' },
      { notice: 'No clusters are registered with this fleet manager yet.' },
      { notice: 'No cluster available.' },
      { href: '/clusters' },
      { notice: 'The fleet manager could not list its clusters.' },
      { notice: 'The fleet manager could not list its clusters.' },
    ]);
  });

  it("sends a path that names no screen to the default cluster's overview, query dropped", () => {
    expect([
      toLanding(fleetOfTwo, hubConfig, '/nope/at/all', '?x=1'),
      toLanding({ status: 'single' }, dashboardConfig, '/clusters', ''),
      toLanding({ status: 'single' }, dashboardConfig, '/operations', '?op=1'),
    ]).toEqual([
      { href: '/c/hub/overview' },
      { href: '/c/local/overview' },
      { href: '/c/local/overview' },
    ]);
  });
});

describe('toCrdGroups', () => {
  it('groups kinds by API group and links each to its first served version', () => {
    expect(toCrdGroups(definitions)).toEqual([
      { group: 'acme.dev', kinds: [{ kind: 'Gadget', plural: 'gadgets', version: 'v1' }] },
      { group: 'example.io', kinds: [{ kind: 'Widget', plural: 'widgets', version: 'v1' }] },
    ]);
  });

  it('labels a kindless definition with its plural and skips one without a group or plural', () => {
    const versions = [{ name: 'v1', served: true, storage: true }];
    const toDefinition = (group: string, kind: string, plural: string) => ({
      spec: { group, scope: 'Cluster', names: { kind, plural }, versions },
    });
    expect(
      toCrdGroups({
        items: [
          toDefinition('acme.dev', '', 'gadgets'),
          toDefinition('', 'Orphan', 'orphans'),
          toDefinition('acme.dev', 'Nameless', ''),
        ],
      }),
    ).toEqual([
      { group: 'acme.dev', kinds: [{ kind: 'gadgets', plural: 'gadgets', version: 'v1' }] },
    ]);
  });
});

describe('toSidebarSections', () => {
  const idsOf = (sections: ReturnType<typeof toSidebarSections>) => sections.map(({ id }) => id);
  const rowsOf = (sections: ReturnType<typeof toSidebarSections>, id: string) =>
    sections.find((section) => section.id === id)?.rows.map(({ label }) => label);
  const hub = toSidebarSections({
    ...hubPlace,
    counts: { nodes: 3 },
    crdGroups: toCrdGroups(definitions),
  });
  const dashboard = toSidebarSections({ ...dashboardPlace, counts: {}, crdGroups: [] });

  it('shows fleet rows and the PRO section only where they apply', () => {
    expect([rowsOf(hub, 'top'), rowsOf(dashboard, 'top')]).toEqual([
      ['Overview', 'Clusters', 'Operations', 'Nodes', 'Namespaces', 'Events'],
      ['Overview', 'Nodes', 'Namespaces', 'Events'],
    ]);
    expect([idsOf(hub).includes('pro'), idsOf(dashboard).includes('pro')]).toEqual([true, false]);
  });

  it('lists only the links the configuration carries', () => {
    expect([rowsOf(hub, 'links'), rowsOf(dashboard, 'links')]).toEqual([
      ['Documentation', 'Grafana'],
      ['Documentation'],
    ]);
    expect(
      idsOf(
        toSidebarSections({
          ...dashboardPlace,
          config: { ...dashboardConfig, links: { docs: '', pro: '' } },
          counts: {},
          crdGroups: [],
        }),
      ).includes('links'),
    ).toBe(false);
  });

  it('carries counts and one sub-section per custom resource group', () => {
    const top = hub.find(({ id }) => id === 'top');
    const crds = hub.find(({ id }) => id === 'crds');
    expect(top?.rows.find(({ id }) => id === 'nodes')?.count).toBe(3);
    expect(crds?.groups.map(({ label, rows }) => [label, rows[0]?.href])).toEqual([
      ['acme.dev', '/c/edge/crds/acme.dev/v1/gadgets?ns=team-a'],
      ['example.io', '/c/edge/crds/example.io/v1/widgets?ns=team-a'],
    ]);
  });

  it('keeps only fleet rows when no cluster can be addressed', () => {
    expect(
      toSidebarSections({ ...hubPlace, clusterId: undefined, counts: {}, crdGroups: [] }).flatMap(
        ({ rows }) => rows.filter(({ isExternal }) => !isExternal).map(({ id }) => id),
      ),
    ).toEqual(['clusters', 'operations']);
  });
});

describe('toCrumbs', () => {
  const trail = (input: Parameters<typeof toCrumbs>[0]) =>
    toCrumbs(input).map(({ label }) => label);

  it('starts with a link to the fleet page against a fleet', () => {
    const crumbs = toCrumbs({
      ...hubPlace,
      clusterName: 'edge',
      location: { clusterId: 'edge', segments: ['helm'] },
    });
    expect(crumbs).toEqual([
      { label: 'Clusters', href: '/clusters' },
      { label: 'edge' },
      { label: 'Helm Releases' },
    ]);
  });

  it('starts with the cluster name, or "Cluster" until it is known, otherwise', () => {
    expect([
      trail({
        ...dashboardPlace,
        clusterName: undefined,
        location: { clusterId: 'local', segments: ['ingress'] },
      }),
      trail({
        ...dashboardPlace,
        clusterName: 'prod',
        location: { clusterId: 'local', segments: ['workloads'] },
      }),
    ]).toEqual([
      ['Cluster', 'Ingresses'],
      ['prod', 'Workloads'],
    ]);
  });

  it('gives a custom resource list a two-step trail ending in the plural from the URL', () => {
    expect(
      trail({
        ...dashboardPlace,
        clusterName: 'prod',
        location: { clusterId: 'local', segments: ['crds', 'example.io', 'v1', 'widgets'] },
      }),
    ).toEqual(['prod', 'Custom Resources', 'widgets']);
  });

  it('ends a custom resource trail in the kind it is given', () => {
    expect(
      trail({
        ...dashboardPlace,
        clusterName: 'prod',
        location: { clusterId: 'local', segments: ['crds', 'example.io', 'v1', 'widgets'] },
        kind: 'Widget',
      }),
    ).toEqual(['prod', 'Custom Resources', 'Widget']);
  });

  it('does not repeat itself on the fleet page and never titles an unknown screen Overview', () => {
    expect([
      trail({ ...hubPlace, clusterName: 'edge', location: { segments: ['clusters'] } }),
      trail({ ...hubPlace, clusterName: 'edge', location: { segments: ['operations'] } }),
      trail({
        ...dashboardPlace,
        clusterName: 'prod',
        location: { clusterId: 'local', segments: ['nope'] },
      }),
    ]).toEqual([['Clusters'], ['Clusters', 'Operations'], ['prod', 'Not found']]);
  });
});

describe('toPaletteEntries', () => {
  const titlesIn = (entries: ReturnType<typeof toPaletteEntries>, group: string) =>
    entries.filter((entry) => entry.group === group).map(({ title }) => title);
  const hub = toPaletteEntries(hubPlace, fleetOfTwo);
  const dashboard = toPaletteEntries(dashboardPlace, { status: 'single' });

  it('starts Navigate with the fixed entries the backend offers', () => {
    expect([
      titlesIn(hub, 'Navigate').slice(0, 11),
      titlesIn(dashboard, 'Navigate').slice(0, 8),
    ]).toEqual([
      [
        'Cluster overview',
        'Clusters',
        'Workloads',
        'Nodes',
        'Events',
        'Helm releases',
        'Storage',
        'RBAC',
        'Ingress',
        'App Catalog',
        'Hardening',
      ],
      [
        'Cluster overview',
        'Workloads',
        'Nodes',
        'Events',
        'Helm releases',
        'Storage',
        'RBAC',
        'Ingress',
      ],
    ]);
  });

  it('lists every resource kind with its singular name when that differs', () => {
    expect(hub.find(({ title }) => title === 'Pods')).toMatchObject({
      subtitle: 'Pod',
      href: '/c/edge/pods?ns=team-a',
    });
    expect(hub.find(({ title }) => title === 'Endpoints')?.subtitle).toBeUndefined();
  });

  it('offers every cluster of a fleet and marks the current one', () => {
    expect(hub.filter(({ group }) => group === 'Switch cluster')).toEqual([
      {
        id: 'cluster:edge',
        group: 'Switch cluster',
        title: 'edge',
        subtitle: 'west · — · 1/2 nodes · degraded',
        href: '/c/edge/overview',
        isCurrent: true,
      },
      {
        id: 'cluster:hub',
        group: 'Switch cluster',
        title: 'hub',
        subtitle: 'Fleet hub · aws · 3/3 nodes · healthy',
        href: '/c/hub/overview',
        isCurrent: false,
      },
    ]);
    expect(titlesIn(dashboard, 'Switch cluster')).toEqual([]);
  });
});

describe('toPaletteEntries with actions', () => {
  it('lists offered actions in an Actions group ahead of the cluster switches', () => {
    const onRun = () => undefined;
    const entries = toPaletteEntries(hubPlace, fleetOfTwo, [
      { id: 'pod-shell', title: 'Open shell in pod…', onRun },
    ]);
    const groups = toPaletteGroups(entries).map(({ group }) => group);
    expect(groups).toEqual(['Navigate', 'Actions', 'Switch cluster']);
    expect(entries.find(({ group }) => group === 'Actions')).toEqual({
      id: 'Actions:pod-shell',
      group: 'Actions',
      title: 'Open shell in pod…',
      onRun,
    });
  });

  it('has no Actions group when nothing is offered', () => {
    expect(toPaletteEntries(hubPlace, fleetOfTwo).some(({ group }) => group === 'Actions')).toBe(
      false,
    );
  });
});

describe('toPaletteGroups', () => {
  it('keeps groups and entries in order', () => {
    const groups = toPaletteGroups(
      toPaletteEntries(hubPlace, { status: 'ready', clusters: [edgeCluster] }),
    );
    expect(groups.map(({ group }) => group)).toEqual(['Navigate', 'Switch cluster']);
    expect(groups[1]?.entries.map(({ title }) => title)).toEqual(['edge']);
  });
});

describe('toClusterRead', () => {
  const state = {
    data: { items: [] },
    fetchFailureCount: 0,
    fetchFailureReason: null,
    dataUpdatedAt: 42,
  };

  it('names a read after the list it asks for', () => {
    expect(
      toClusterRead({ queryKey: ['k8s', 'edge', 'api', 'v1', 'pods', { limit: 5 }], state }),
    ).toEqual({
      name: 'pods',
      isClusterRead: true,
      hasData: true,
      failureCount: 0,
      isRefused: false,
      answeredAt: 42,
    });
  });

  it('tells a refusal from a read that got no answer', () => {
    const failing = (fetchFailureReason: unknown) =>
      toClusterRead({
        queryKey: ['k8s', 'edge', 'api', 'v1', 'secrets'],
        state: { ...state, data: undefined, fetchFailureCount: 1, fetchFailureReason },
      }).isRefused;
    expect([failing({ status: 403, payload: {} }), failing({ status: 503, payload: {} })]).toEqual([
      true,
      false,
    ]);
  });
});

describe('toTrailKind', () => {
  it('is the definition kind, the plural while resolving, and a guess once unreadable', () => {
    expect([
      toTrailKind('widgets', { kind: 'Widget', isPending: false }),
      toTrailKind('widgets', { isPending: true }),
      toTrailKind('widgets', { isPending: false }),
      toTrailKind('policies', { isPending: false }),
    ]).toEqual(['Widget', 'widgets', 'Widget', 'Policy']);
  });
});

describe('toLocalCluster', () => {
  it('names the one cluster "local" once its identity is known', () => {
    expect([toLocalCluster(true)?.name, toLocalCluster(false)]).toEqual(['local', undefined]);
  });
});

describe('toClusterRead', () => {
  const state = { data: {}, fetchFailureCount: 0, fetchFailureReason: null, dataUpdatedAt: 5 };

  it('names a read by its path, or by the name its owner gave it', () => {
    expect([
      toClusterRead({ queryKey: ['k8s', 'edge', 'api', 'v1', 'pods', {}], state }),
      toClusterRead({
        queryKey: ['api', 'v1', 'secret-summaries'],
        meta: { readName: 'HELM' },
        state,
      }),
    ]).toMatchObject([
      { name: 'pods', isClusterRead: true },
      { name: 'HELM', isClusterRead: false },
    ]);
  });
});
