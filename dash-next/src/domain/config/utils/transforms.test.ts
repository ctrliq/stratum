import { describe, expect, it } from 'vitest';

import {
  toAutoscalerRow,
  toConfigMapRow,
  toDisruptionBudgetRow,
  toLeaseRow,
  toLimitRangeRow,
  toPriorityClassRow,
  toResourceQuotaRow,
  toRuntimeClassRow,
  toSecretListings,
  toSecretRow,
  toWebhookRow,
} from './transforms';

describe('toConfigMapRow', () => {
  it('carries the number of keys', () => {
    expect(toConfigMapRow({ metadata: { name: 'settings' }, data: { mode: 'dark' } })).toEqual({
      keys: 1,
    });
  });
});

describe('toSecretListings', () => {
  it('reads each summary as metadata, type and key count', () => {
    expect(
      toSecretListings({
        items: [
          {
            name: 'db-credentials',
            namespace: 'payments',
            type: 'Opaque',
            keys: 2,
            creationTimestamp: '2026-01-10T12:00:00Z',
          },
          {
            name: 'tls',
            namespace: 'web',
            type: 'kubernetes.io/tls',
            keys: 2,
            creationTimestamp: null,
          },
        ],
      }),
    ).toEqual({
      items: [
        {
          metadata: {
            name: 'db-credentials',
            namespace: 'payments',
            creationTimestamp: '2026-01-10T12:00:00Z',
          },
          type: 'Opaque',
          keyCount: 2,
        },
        {
          metadata: { name: 'tls', namespace: 'web', creationTimestamp: undefined },
          type: 'kubernetes.io/tls',
          keyCount: 2,
        },
      ],
    });
  });
});

describe('toSecretRow', () => {
  it('shows the type and the number of keys, "Opaque" when the type is unset', () => {
    expect([
      toSecretRow({ type: 'kubernetes.io/tls', keyCount: 2 }),
      toSecretRow({ keyCount: 0 }),
    ]).toEqual([
      { type: 'kubernetes.io/tls', keys: 2 },
      { type: 'Opaque', keys: 0 },
    ]);
  });
});

describe('toResourceQuotaRow', () => {
  it('joins the scopes and counts the hard limits', () => {
    expect([
      toResourceQuotaRow({
        spec: { scopes: ['BestEffort', 'NotTerminating'] },
        status: { hard: { pods: '10' } },
      }),
      toResourceQuotaRow({}),
    ]).toEqual([
      { scopes: 'BestEffort, NotTerminating', hardLimits: 1 },
      { scopes: '—', hardLimits: 0 },
    ]);
  });
});

describe('toLimitRangeRow', () => {
  it('counts the limit entries in spec', () => {
    expect([
      toLimitRangeRow({ spec: { limits: [{ type: 'Container' }, { type: 'Pod' }] } }),
      toLimitRangeRow({}),
    ]).toEqual([{ limits: 2 }, { limits: 0 }]);
  });
});

describe('toAutoscalerRow', () => {
  it('reads the scale target, the bounds and the current replicas', () => {
    expect(
      toAutoscalerRow({
        spec: {
          scaleTargetRef: { kind: 'Deployment', name: 'api' },
          minReplicas: 2,
          maxReplicas: 8,
        },
        status: { currentReplicas: 3, desiredReplicas: 5 },
      }),
    ).toEqual({ reference: 'Deployment/api', minReplicas: '2', maxReplicas: '8', replicas: 3 });
  });

  it('writes "?" for a missing part of the target and marks unset bounds', () => {
    expect([
      toAutoscalerRow({ spec: { scaleTargetRef: { kind: '', name: 'api' }, maxReplicas: 8 } }),
      toAutoscalerRow({}),
    ]).toEqual([
      { reference: '?/api', minReplicas: '—', maxReplicas: '8', replicas: 0 },
      { reference: '?/?', minReplicas: '—', maxReplicas: '—', replicas: 0 },
    ]);
  });
});

describe('toDisruptionBudgetRow', () => {
  it('shows a number or a percentage for each bound and the healthy count', () => {
    expect([
      toDisruptionBudgetRow({
        spec: { minAvailable: 2 },
        status: { currentHealthy: 3, desiredHealthy: 2, disruptionsAllowed: 1, expectedPods: 3 },
      }),
      toDisruptionBudgetRow({ spec: { maxUnavailable: '25%' } }),
    ]).toEqual([
      { minAvailable: '2', maxUnavailable: '—', currentHealthy: 3 },
      { minAvailable: '—', maxUnavailable: '25%', currentHealthy: 0 },
    ]);
  });
});

describe('toPriorityClassRow', () => {
  it('shows the value and whether the class is the global default', () => {
    expect([
      toPriorityClassRow({ value: 1000, globalDefault: true }),
      toPriorityClassRow({ value: 0 }),
    ]).toEqual([
      { value: '1000', globalDefault: 'true' },
      { value: '0', globalDefault: 'false' },
    ]);
  });
});

describe('toRuntimeClassRow', () => {
  it('shows the handler', () => {
    expect(toRuntimeClassRow({ handler: 'runsc' })).toEqual({ handler: 'runsc' });
  });
});

describe('toLeaseRow', () => {
  it('shows the holder, or the absent mark when nobody holds the lease', () => {
    expect([toLeaseRow({ spec: { holderIdentity: 'node-1' } }), toLeaseRow({})]).toEqual([
      { holder: 'node-1' },
      { holder: '—' },
    ]);
  });
});

describe('toWebhookRow', () => {
  it('counts the webhooks in the configuration', () => {
    expect(toWebhookRow({ webhooks: [{ name: 'a' }] })).toEqual({ webhooks: 1 });
  });
});
