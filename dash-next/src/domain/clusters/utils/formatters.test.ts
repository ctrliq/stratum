import { describe, expect, it } from 'vitest';

import { deployNameHelp, registerNameHelp } from '../constants';
import { toCluster, toRefusal } from '../testing/fixtures';
import {
  toActionError,
  toClusterCountText,
  toContext,
  toCoverageText,
  toDenialText,
  toDeployRefusal,
  toDestroyFacts,
  toDestroyTooltip,
  toEngineStateRefusal,
  toEvaluatedText,
  toFindingCountsText,
  toFreshnessTone,
  toHandlerText,
  toKubeconfigRefusal,
  toNameHint,
  toNodesText,
  toNoMatchText,
  toObservedText,
  toPlacementHint,
  toProtectionToast,
  toProtectionTooltip,
  toQuotedRefusal,
  toRefusalNote,
  toSeverityTone,
  toSummaryLine,
  toTokenRefusal,
  toUnknownCoverageText,
  toUnregisterError,
  toVersionHint,
  toVersionText,
} from './formatters';

const now = Date.parse('2026-11-13T12:00:00Z');
const before = (seconds: number): string => new Date(now - seconds * 1000).toISOString();
const fleet = [toCluster()];

describe('toContext', () => {
  it.each([
    ['Production', {}, 'Production'],
    ['Production', { env: 'live' }, 'Production'],
    ['prod', { env: 'live' }, 'live'],
    ['', { env: 'live', tier: 'gold' }, 'live'],
    ['prod', {}, 'prod'],
    ['', {}, 'prod'],
  ])('display name %j with labels %j reads %s', (displayName, labels, context) => {
    expect(toContext({ id: 'prod', name: 'prod', displayName, labels })).toBe(context);
  });

  it('falls back to the id', () => {
    expect(toContext({ id: 'c-1', name: '', displayName: '', labels: {} })).toBe('c-1');
  });
});

describe('inventory facts', () => {
  it.each([
    ['1.34.0', 'v1.34.0'],
    ['v1.34.0', 'v1.34.0'],
    ['', '—'],
  ])('shows the version %j as %s', (reported, shown) => {
    expect(toVersionText(reported)).toBe(shown);
  });

  it('shows ready over total nodes, 0/0 when unreported', () => {
    expect([toNodesText({ readyNodes: 5, nodeCount: 6 }), toNodesText({})]).toEqual(['5/6', '0/0']);
  });
});

describe('toSummaryLine', () => {
  it('counts healthy clusters and shows a dash for the context a fleet page never has', () => {
    expect([toSummaryLine(2, 3), toSummaryLine(0, 0)]).toEqual([
      '2 of 3 healthy · context: —',
      '0 of 0 healthy · context: —',
    ]);
  });
});

describe('toNoMatchText', () => {
  it('quotes the raw search text', () => {
    expect(toNoMatchText('zzz')).toBe('No clusters match "zzz".');
  });
});

describe('toNameHint', () => {
  it.each([
    ['', registerNameHelp, false],
    ['staging', registerNameHelp, false],
    ['Staging', 'lowercase DNS-1123 label: a-z, 0-9, "-" (max 63)', true],
    ['-staging', 'lowercase DNS-1123 label: a-z, 0-9, "-" (max 63)', true],
    ['prod', 'already registered', true],
  ])('for %j reads %s', (name, text, isError) => {
    expect(toNameHint(name, fleet, registerNameHelp)).toEqual({ text, isError });
  });

  it('uses the help text of the form it serves', () => {
    expect(toNameHint('staging-eu', fleet, deployNameHelp).text).toBe(deployNameHelp);
  });
});

describe('toVersionHint and toPlacementHint', () => {
  it.each([
    ['v1.34.0', 'the version ClusterAPI will build', false],
    ['', 'the version ClusterAPI will build', false],
    ['1.34.0', 'must look like v1.34.0', true],
  ])('version %j reads %s', (version, text, isError) => {
    expect(toVersionHint(version)).toEqual({ text, isError });
  });

  it('follows the validity of the placement text', () => {
    expect([toPlacementHint('a=1').isError, toPlacementHint('oops')]).toEqual([
      false,
      { text: 'each line must be key=value', isError: true },
    ]);
  });
});

describe('refusals', () => {
  it('explains a reserved name on a token request and quotes anything else', () => {
    expect([
      toTokenRefusal(toRefusal(409, 'name-taken', 'taken'), 'staging'),
      toTokenRefusal(toRefusal(403, 'insufficient-role', 'admin role required'), 'staging'),
    ]).toEqual([
      'The name "staging" is already reserved by a cluster or an outstanding token.',
      'admin role required',
    ]);
  });

  it.each([
    ['name-taken', 'already exists', 'The name "edge" is already taken.'],
    [
      'member-unreachable',
      'dial tcp: i/o timeout',
      'The fleet manager could not reach that cluster: dial tcp: i/o timeout',
    ],
    ['bad-request', 'exec plugins refused', 'exec plugins refused'],
    ['insufficient-role', 'admin role required', 'admin role required'],
  ])('explains the kubeconfig refusal %s', (code, message, shown) => {
    expect(toKubeconfigRefusal(toRefusal(400, code, message), 'edge')).toBe(shown);
  });

  it('answers a hub without provisioning with a fixed message', () => {
    const disabled = 'Provisioning via ClusterAPI is not enabled in this release';
    expect([
      toDeployRefusal(toRefusal(501, 'x', 'y')),
      toDeployRefusal(toRefusal(409, 'name-taken', 'the name is taken')),
      toActionError(toRefusal(400, 'capi-not-enabled', 'y')),
      toActionError(toRefusal(403, 'insufficient-role', 'admin only')),
      toActionError({ status: 'unknown', payload: 'Failed to fetch' }),
    ]).toEqual([
      disabled,
      'the name is taken',
      disabled,
      'Error: admin only',
      'Error: Failed to fetch',
    ]);
  });

  it('says the fleet manager cluster cannot be unregistered', () => {
    expect([
      toUnregisterError(toRefusal(409, 'fleet-manager-cluster', 'no')),
      toUnregisterError(toRefusal(404, 'not-found', 'the cluster is gone')),
    ]).toEqual(['The fleet manager cluster cannot be unregistered', 'Error: the cluster is gone']);
  });
});

describe('destroy and protection wording', () => {
  it.each([
    [false, true, false, 'Destroy cluster (ClusterAPI)'],
    [false, true, true, 'Cannot destroy — deletion protection is on'],
    [
      false,
      false,
      true,
      'Cannot destroy — this cluster was registered, not built by the fleet manager',
    ],
    [true, true, true, 'Cannot destroy — this cluster runs the fleet manager'],
  ])(
    'fleet manager %s, built %s, protected %s',
    (fleetManager, provisioned, deletionProtected, tooltip) => {
      expect(toDestroyTooltip({ fleetManager, provisioned, deletionProtected })).toBe(tooltip);
    },
  );

  it('states what a click on the protection control does and what the hub answered', () => {
    expect([
      toProtectionTooltip({ deletionProtected: true }),
      toProtectionTooltip({ deletionProtected: false }),
      toProtectionToast('lab', true),
      toProtectionToast('lab', false),
    ]).toEqual([
      'Remove destroy protection',
      'Protect from destroy',
      'lab is now protected from destroy',
      'Removed destroy protection from lab',
    ]);
  });

  it('shows the provider and node count, with placeholders and a build note', () => {
    expect([
      toDestroyFacts({ provider: 'proxmox', nodeCount: 4, status: 'healthy' }),
      toDestroyFacts({ provider: '', status: 'provisioning' }),
    ]).toEqual([
      'Provider: proxmox · Nodes: 4',
      'Provider: — · Nodes: 0 (still being built — machines may not exist yet)',
    ]);
  });
});

describe('roll-up wording', () => {
  it.each([
    [1, 1, '1 cluster', '1 cluster with unknown required coverage'],
    [0, 0, '0 clusters', '0 clusters with unknown required coverage'],
    [7, 3, '7 clusters', '3 clusters with unknown required coverage'],
  ])('%s clusters, %s unknown', (clusterCount, requiredUnknownClusters, clusters, coverage) => {
    expect([
      toClusterCountText({ clusterCount }),
      toUnknownCoverageText({ requiredUnknownClusters }),
    ]).toEqual([clusters, coverage]);
  });

  it('shows placeholders for missing counts', () => {
    expect([toClusterCountText({}), toUnknownCoverageText({})]).toEqual([
      '— clusters',
      '0 clusters with unknown required coverage',
    ]);
  });

  it('prints coverage and finding counts as the hub states them', () => {
    expect([
      toCoverageText({ evaluated: 8, expected: 10 }),
      toCoverageText(undefined),
      toFindingCountsText({ total: 5, required: 3, optional: 2 }),
    ]).toEqual(['8/10 evaluated', '—/— evaluated', '5 total · 3 required · 2 optional']);
  });
});

describe('tones', () => {
  it.each([
    ['healthy', 'success'],
    ['info', 'progress'],
    ['unknown', 'neutral'],
    ['warning', 'warning'],
    ['critical', 'error'],
    ['other', 'neutral'],
  ])('severity %s uses the %s tone', (severity, tone) => {
    expect(toSeverityTone(severity)).toBe(tone);
  });

  it.each([
    ['fresh', 'success'],
    ['stale', 'warning'],
    ['warning', 'warning'],
    ['degraded', 'error'],
    ['unknown', 'neutral'],
    ['other', 'neutral'],
  ])('freshness %s uses the %s tone', (freshness, tone) => {
    expect(toFreshnessTone(freshness)).toBe(tone);
  });
});

describe('capability wording', () => {
  it.each([
    [
      { status: 404, payload: { error: 'not-found', message: 'no cluster edge' } },
      'capability record unavailable — no cluster edge',
    ],
    [
      { status: 503, payload: { error: 'busy', message: 'hub overloaded', retryable: true } },
      'capability record unavailable — hub overloaded · retryable',
    ],
  ])('quotes the refusal %j', (error, note) => {
    expect(toRefusalNote('capability record unavailable', error)).toBe(note);
  });

  it('prints the handler version and the denial reason when the record gives them', () => {
    expect([
      toHandlerText({ handlerVersion: '1.4.2' }),
      toHandlerText({ handlerVersion: null }),
      toDenialText({ reason: { code: 'disconnected', message: 'no connected session' } }),
      toDenialText({ reason: null }),
    ]).toEqual(['handler 1.4.2', undefined, 'disconnected: no connected session', undefined]);
  });
});

describe('toEvaluatedText and toObservedText', () => {
  it('cannot be computed for a missing, unparseable or future time', () => {
    expect(
      [undefined, 'garbage', before(-30)].map((timestamp) => toEvaluatedText(timestamp, now)),
    ).toEqual(['evaluated —', 'evaluated —', 'evaluated —']);
  });

  it('follows the observation time with its age, or shows a dash', () => {
    expect([toObservedText(before(30), now), toObservedText(null, now)]).toEqual([
      `${new Date(before(30)).toLocaleString()} (30s ago)`,
      '—',
    ]);
  });
});

describe('toEngineStateRefusal', () => {
  it('names a read that ran out of time and marks it retryable', () => {
    expect([
      toEngineStateRefusal('the latest read was refused', {
        status: 'unknown',
        payload: 'signal timed out',
        reason: 'timeout',
      }),
      toEngineStateRefusal('the latest read was refused', {
        status: 503,
        payload: { message: 'hub overloaded' },
      }),
    ]).toEqual([
      'the latest read was refused — the engine-state refresh timed out · retryable',
      'the latest read was refused — hub overloaded',
    ]);
  });
});

describe('toQuotedRefusal', () => {
  it('quotes the hub after the lead', () => {
    expect(
      toQuotedRefusal('fleet engine state unavailable', {
        status: 403,
        payload: { error: 'insufficient-role', message: 'insufficient role' },
      }),
    ).toBe('fleet engine state unavailable — insufficient role');
  });
});
