import { describe, expect, it } from 'vitest';

import { dashboardConfig, hubConfig, standaloneHubConfig } from '~/domain/auth/testing/fixtures';

import { screens } from '../constants';
import type { ClusterRead, Fleet, Screen } from '../types/screen';
import {
  isClusterKnown,
  isClusterUnreachable,
  isCrdGroupOpen,
  isCustomResourceList,
  isFactsRead,
  isFirstLoad,
  isHubRead,
  isLegacyScreenPath,
  isManuallyRefreshed,
  isPaletteShortcut,
  isReadFailure,
  isReconnecting,
  isRefreshFailed,
  isScreenOffered,
  isSectionCollapsed,
  isShowingStaleData,
} from './predicates';

const screenWithId = (id: string): Screen =>
  screens.find((screen) => screen.id === id) ?? screens[0];

const read = (patch: Partial<ClusterRead>): ClusterRead => ({
  name: 'pods',
  isClusterRead: true,
  hasData: true,
  failureCount: 0,
  isRefused: false,
  answeredAt: 1000,
  ...patch,
});

const waiting = read({ hasData: false, answeredAt: 0 });
const down = read({ hasData: false, failureCount: 2, answeredAt: 0 });
const stale = read({ failureCount: 1 });
const forbidden = read({ hasData: false, failureCount: 1, isRefused: true, answeredAt: 0 });

describe('isScreenOffered', () => {
  it('offers the fleet rows in fleet mode only', () => {
    expect(
      [hubConfig, standaloneHubConfig, dashboardConfig].map((config) =>
        isScreenOffered(screenWithId('clusters'), config),
      ),
    ).toEqual([true, false, false]);
  });

  it('offers the hub-only screens against a hub in either mode', () => {
    expect(
      [hubConfig, standaloneHubConfig, dashboardConfig].map((config) =>
        isScreenOffered(screenWithId('hardening'), config),
      ),
    ).toEqual([true, true, false]);
  });

  it('offers a cluster screen everywhere', () => {
    expect(isScreenOffered(screenWithId('pods'), dashboardConfig)).toBe(true);
  });
});

describe('isPaletteShortcut', () => {
  it('is K with Command or Control, in either case', () => {
    expect(
      [
        { key: 'k', metaKey: true, ctrlKey: false },
        { key: 'K', metaKey: false, ctrlKey: true },
        { key: 'k', metaKey: false, ctrlKey: false },
        { key: 'j', metaKey: true, ctrlKey: false },
      ].map(isPaletteShortcut),
    ).toEqual([true, true, false, false]);
  });
});

describe('isClusterKnown', () => {
  const fleet: Fleet = {
    status: 'ready',
    clusters: [
      {
        id: 'hub',
        name: 'hub',
        displayName: 'hub',
        labels: {},
        provider: '',
        k8sVersion: '',
        fleetManager: true,
        nodeCount: 1,
        readyNodes: 1,
        status: 'healthy',
      },
    ],
  };

  it('knows only "local" on a single-cluster backend', () => {
    expect([
      isClusterKnown({ status: 'single' }, 'local'),
      isClusterKnown({ status: 'single' }, 'edge'),
    ]).toEqual([true, false]);
  });

  it('knows the clusters the hub lists', () => {
    expect([isClusterKnown(fleet, 'hub'), isClusterKnown(fleet, 'gone')]).toEqual([true, false]);
  });

  it('cannot rule a cluster out while the list is unreadable', () => {
    expect(isClusterKnown({ status: 'unreadable' }, 'edge')).toBe(true);
  });
});

describe('isCustomResourceList', () => {
  it('is a path of group, version and plural under crds', () => {
    expect(
      [['crds', 'example.io', 'v1', 'widgets'], ['crds'], ['pods']].map((segments) =>
        isCustomResourceList({ clusterId: 'a', segments }),
      ),
    ).toEqual([true, false, false]);
  });
});

describe('isSectionCollapsed', () => {
  it('is expanded unless the user collapsed it', () => {
    expect([
      isSectionCollapsed({ workloads: true }, 'workloads'),
      isSectionCollapsed({ workloads: false }, 'workloads'),
      isSectionCollapsed({}, 'workloads'),
    ]).toEqual([true, false, false]);
  });
});

describe('isCrdGroupOpen', () => {
  it('opens by itself only for the current page', () => {
    expect([
      isCrdGroupOpen({}, 'example.io', true),
      isCrdGroupOpen({}, 'example.io', false),
    ]).toEqual([true, false]);
  });

  it("lets the user's stored choice win", () => {
    expect([
      isCrdGroupOpen({ 'crd:example.io': true }, 'example.io', true),
      isCrdGroupOpen({ 'crd:example.io': false }, 'example.io', false),
    ]).toEqual([false, true]);
  });
});

describe('isShowingStaleData', () => {
  it('is old data kept after a refresh that got no answer', () => {
    expect([stale, read({}), down, forbidden].map(isShowingStaleData)).toEqual([
      true,
      false,
      false,
      false,
    ]);
  });
});

describe('isFirstLoad', () => {
  it('lasts until any list has loaded, whatever failed meanwhile', () => {
    expect([
      isFirstLoad([waiting, waiting]),
      isFirstLoad([waiting, read({})]),
      isFirstLoad([down, waiting]),
      isFirstLoad([]),
    ]).toEqual([true, false, true, false]);
  });
});

describe('isClusterUnreachable', () => {
  it('is when lists fail and nothing answers', () => {
    expect([isClusterUnreachable([down, stale]), isClusterUnreachable([down, waiting])]).toEqual([
      true,
      true,
    ]);
  });

  it('is not when any list answered, even with a refusal', () => {
    expect([
      isClusterUnreachable([stale, read({})]),
      isClusterUnreachable([down, read({ failureCount: 1, isRefused: true })]),
      isClusterUnreachable([waiting]),
      isClusterUnreachable([]),
    ]).toEqual([false, false, false, false]);
  });
});

describe('isManuallyRefreshed', () => {
  const polled = [{ options: { staleTime: 15000 } }];

  it("covers the selected cluster's Kubernetes reads and every hub read, not another cluster's", () => {
    expect(
      [
        ['k8s', 'edge', 'api', 'v1', 'pods'],
        ['k8s', 'hub', 'api', 'v1', 'pods'],
        ['api', 'v1', 'clusters', 'edge', 'engine-state'],
      ].map((queryKey) => isManuallyRefreshed('edge')({ queryKey, observers: polled })),
    ).toEqual([true, false, true]);
  });

  it('leaves alone a read every reader keeps for good', () => {
    const queryKey = ['k8s', 'edge', 'apis', 'apiextensions.k8s.io', 'v1', 'widgets.example.io'];
    expect([
      isManuallyRefreshed('edge')({ queryKey, observers: [{ options: { staleTime: Infinity } }] }),
      isManuallyRefreshed('edge')({
        queryKey,
        observers: [{ options: { staleTime: Infinity } }, ...polled],
      }),
    ]).toEqual([false, true]);
  });

  it('leaves alone a read whose reader says it has settled', () => {
    const settled = { queryKey: ['api', 'v1', 'operations', 'op-1'], isSettled: true };
    const freshFor = (read: typeof settled): number => (read.isSettled ? Infinity : 15000);
    expect(
      isManuallyRefreshed(undefined)({
        ...settled,
        observers: [{ options: { staleTime: freshFor } }],
      }),
    ).toBe(false);
  });
});

describe('isReconnecting', () => {
  it('raises the banner on the hub only, for a selected cluster, while live', () => {
    expect([
      isReconnecting(hubConfig, true, 'edge', [down, stale]),
      isReconnecting(dashboardConfig, true, 'local', [down, stale]),
      isReconnecting(hubConfig, false, 'edge', [down, stale]),
      isReconnecting(hubConfig, true, undefined, [down, stale]),
      isReconnecting(hubConfig, true, 'edge', [stale, read({})]),
    ]).toEqual([true, false, false, false, false]);
  });
});

describe('isRefreshFailed', () => {
  const hubRead = read({ name: 'clusters', isClusterRead: false });

  it('is when no read of the selected cluster answered, whatever the hub answered', () => {
    expect([
      isRefreshFailed([down, stale, hubRead]),
      isRefreshFailed([down, down]),
      isRefreshFailed([stale, read({})]),
      isRefreshFailed([down, read({ failureCount: 1, isRefused: true })]),
    ]).toEqual([true, true, false, false]);
  });

  it('judges the hub reads on a page without a cluster, and nothing at all as fine', () => {
    expect([
      isRefreshFailed([{ ...hubRead, failureCount: 2 }]),
      isRefreshFailed([hubRead]),
      isRefreshFailed([]),
    ]).toEqual([true, false, false]);
  });
});

describe('isFactsRead and isHubRead', () => {
  it('tell the hub facts read and hub reads from Kubernetes reads', () => {
    expect([
      isFactsRead(['api', 'v1', 'clusters', 'edge', 'facts']),
      isFactsRead(['k8s', 'edge', 'api', 'v1', 'facts']),
      isHubRead({ queryKey: ['api', 'v1', 'clusters'] }),
      isHubRead({ queryKey: ['k8s', 'edge', 'api', 'v1', 'pods'] }),
      isHubRead({ queryKey: ['api', 'v1', 'me'] }),
    ]).toEqual([true, false, true, false, false]);
  });
});

describe('isLegacyScreenPath', () => {
  it('is a top-level path that names a cluster screen', () => {
    expect(
      [['pods'], ['crds', 'example.io'], ['clusters'], ['nope'], []].map(isLegacyScreenPath),
    ).toEqual([true, true, false, false, false]);
  });
});

describe('isReadFailure', () => {
  it('is a failed attempt or a final failure of a read', () => {
    expect(['failed', 'error', 'success', 'fetch'].map((type) => isReadFailure({ type }))).toEqual([
      true,
      true,
      false,
      false,
    ]);
  });
});
