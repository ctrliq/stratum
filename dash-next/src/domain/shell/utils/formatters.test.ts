import { describe, expect, it } from 'vitest';

import {
  admin,
  dashboardConfig,
  hubConfig,
  serviceAccount,
  viewer,
  withoutSignIn,
} from '~/domain/auth/testing/fixtures';

import { edgeCluster, hubCluster, read } from '../testing/fixtures';
import {
  toButtonName,
  toClusterCaption,
  toClusterSubtitle,
  toConfigFailure,
  toIdentityLine,
  toLiveLabel,
  toReadWarning,
  toRefreshMessage,
  toSplash,
  toSwitcherHint,
  toUserName,
  toVersionLine,
} from './formatters';

describe('toConfigFailure', () => {
  it('names the request and what went wrong', () => {
    expect([
      toConfigFailure({ status: 502, payload: 'Bad Gateway' }),
      toConfigFailure({ status: 'unknown', payload: 'Failed to fetch' }),
      toConfigFailure({ status: 200, payload: 'Unexpected token <', reason: 'unreadable' }),
      toConfigFailure(new Error('invalid')),
    ]).toEqual([
      'GET /api/v1/config -> HTTP 502',
      'GET /api/v1/config -> Failed to fetch',
      'GET /api/v1/config -> Unexpected token <',
      'GET /api/v1/config returned an unsupported or malformed configuration',
    ]);
  });
});

describe('toSplash', () => {
  it('names the backend the configuration describes', () => {
    expect([toSplash(dashboardConfig), toSplash(hubConfig)]).toEqual([
      'Connecting to the cluster…',
      'Connecting to the fleet manager…',
    ]);
  });
});

describe('toUserName', () => {
  it('is the signed-in user, a dash when unknown, or the proxy without sign-in', () => {
    expect([
      toUserName(hubConfig, admin),
      toUserName(hubConfig, undefined),
      toUserName(withoutSignIn(dashboardConfig), undefined),
    ]).toEqual(['ada', '—', 'kubectl proxy']);
  });
});

describe('toButtonName', () => {
  it.each([
    ['system:serviceaccount:ops:robot', 'robot'],
    ['alice', 'alice'],
    ['odd:name:', 'odd:name:'],
  ])('shows %s as %s', (identity, shown) => {
    expect(toButtonName(identity)).toBe(shown);
  });
});

describe('toIdentityLine', () => {
  it('lists user, roles and provider, leaving out what is unknown', () => {
    expect([
      toIdentityLine(hubConfig, viewer),
      toIdentityLine(dashboardConfig, serviceAccount),
      toIdentityLine(withoutSignIn(dashboardConfig), undefined),
    ]).toEqual([
      'vic · viewer · oidc',
      'system:serviceaccount:ops:robot · kubernetes',
      'kubectl proxy',
    ]);
  });
});

describe('toVersionLine', () => {
  it('names the product by edition and adds Kubernetes with one leading v', () => {
    expect([
      toVersionLine(dashboardConfig, 'v1.36.4'),
      toVersionLine(hubConfig, '1.36.4'),
      toVersionLine(hubConfig, undefined),
      toVersionLine({ ...dashboardConfig, version: '' }, ''),
    ]).toEqual([
      'Stratum v1.2.3 · Kubernetes v1.36.4',
      'Stratum PRO v0.9.0 · Kubernetes v1.36.4',
      'Stratum PRO v0.9.0',
      'Stratum',
    ]);
  });
});

describe('toClusterCaption and toClusterSubtitle', () => {
  it('prefer a distinct display name, then the first label, then the name', () => {
    expect([
      toClusterCaption(hubCluster),
      toClusterCaption(edgeCluster),
      toClusterCaption({ ...edgeCluster, labels: {} }),
    ]).toEqual(['Fleet hub · aws', 'west · —', 'edge · —']);
  });

  it('add node counts and status for the palette', () => {
    expect([
      toClusterSubtitle(edgeCluster),
      toClusterSubtitle({ ...edgeCluster, status: undefined }),
      toClusterSubtitle({
        ...edgeCluster,
        readyNodes: undefined,
        nodeCount: undefined,
        status: undefined,
      }),
    ]).toEqual([
      'west · — · 1/2 nodes · degraded',
      'west · — · 1/2 nodes · —',
      'west · — · 0/0 nodes · —',
    ]);
  });

  it('hint at what the switcher does', () => {
    expect([toSwitcherHint(edgeCluster), toSwitcherHint(undefined)]).toEqual([
      'Switch cluster — current: edge',
      'Choose a cluster',
    ]);
  });
});

describe('toLiveLabel', () => {
  const view = { isLive: true, refreshedAt: 0, staleLists: [], now: 10_000 };

  it('only claims what happened', () => {
    expect([
      toLiveLabel(view),
      toLiveLabel({ ...view, refreshedAt: 4000 }),
      toLiveLabel({ ...view, refreshedAt: 4000, staleLists: ['pods', 'nodes'] }),
      toLiveLabel({ ...view, isLive: false }),
    ]).toEqual([
      'Live — waiting for the first refresh; click to pause',
      'Live — refreshed 6s ago; click to pause',
      'Live — data stale (pods, nodes); click to pause',
      'Paused — click to resume auto-refresh',
    ]);
  });

  it('shortens a long list of stale data', () => {
    expect(toLiveLabel({ ...view, staleLists: ['a', 'b', 'c', 'd', 'e', 'f'] })).toBe(
      'Live — data stale (a, b, c, d +2 more); click to pause',
    );
  });
});

describe('toRefreshMessage', () => {
  const down = read({ failureCount: 2 });
  const neverLoaded = read({ hasData: false, failureCount: 1 });
  const hubRead = read({ name: 'clusters', isClusterRead: false });

  it('reports a refresh precisely', () => {
    expect([
      toRefreshMessage([read(), read({ name: 'nodes' }), hubRead]),
      toRefreshMessage([down, read({ name: 'nodes' })]),
      toRefreshMessage([down, { ...down, name: 'nodes' }]),
    ]).toEqual([
      'Refreshed',
      'Refreshed — pods kept last data',
      'Refresh failed — keeping last data',
    ]);
  });

  it('fails when the cluster did not answer although the hub did, and on a first load', () => {
    expect([
      toRefreshMessage([down, hubRead]),
      toRefreshMessage([neverLoaded, neverLoaded]),
    ]).toEqual(['Refresh failed — keeping last data', 'Refresh failed — keeping last data']);
  });

  it('counts reads, not names, when several share one', () => {
    const summary = read({ name: 'summary', failureCount: 1 });
    expect(toRefreshMessage([summary, summary, { ...down, name: 'nodes' }])).toBe(
      'Refresh failed — keeping last data',
    );
  });

  it('names a hub read that kept its last data', () => {
    expect(toRefreshMessage([read(), { ...hubRead, name: 'HELM', failureCount: 1 }])).toBe(
      'Refreshed — HELM kept last data',
    );
  });

  it('shortens a long list of stale names', () => {
    const names = ['NODES', 'FLEET', 'NODE_CONDITIONS', 'PODS', 'CLUSTER_INFO', 'CLUSTER'];
    expect(
      toRefreshMessage([read(), ...names.map((name) => read({ name, failureCount: 1 }))]),
    ).toBe('Refreshed — NODES, FLEET, NODE_CONDITIONS, PODS +2 more kept last data');
  });
});

describe('toReadWarning', () => {
  it('names the path and the failure', () => {
    expect([
      toReadWarning(['k8s', 'edge', 'apis', 'batch', 'v1', 'jobs', { limit: 5 }], {
        status: 503,
        payload: { message: 'etcd unavailable' },
      }),
      toReadWarning(['api', 'v1', 'clusters', 'edge', 'facts'], { status: 502, payload: '' }),
      toReadWarning(['k8s', 'edge', 'api', 'v1', 'pods'], {
        status: 'unknown',
        payload: 'Failed to fetch',
      }),
    ]).toEqual([
      'Stratum live: /apis/batch/v1/jobs -> HTTP 503: etcd unavailable',
      'Stratum live: /api/v1/clusters/edge/facts -> HTTP 502',
      'Stratum live: /api/v1/pods -> Failed to fetch',
    ]);
  });
});
