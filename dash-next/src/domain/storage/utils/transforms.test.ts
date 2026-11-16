import { describe, expect, it } from 'vitest';

import { toClaimRow, toStorageClassRow, toVolumeRow } from './transforms';

describe('toStorageClassRow', () => {
  it('applies the Kubernetes defaults', () => {
    expect(
      toStorageClassRow({ metadata: { name: 'local-path' }, provisioner: 'rancher.io/local-path' }),
    ).toEqual({
      name: 'local-path',
      provisioner: 'rancher.io/local-path',
      reclaimPolicy: 'Delete',
      bindingMode: 'Immediate',
      allowsExpansion: false,
      isDefault: false,
    });
  });

  it('carries what the class states and marks the default class', () => {
    expect(
      toStorageClassRow({
        metadata: {
          name: 'longhorn',
          annotations: { 'storageclass.kubernetes.io/is-default-class': 'true' },
        },
        provisioner: 'driver.longhorn.io',
        reclaimPolicy: 'Retain',
        volumeBindingMode: 'WaitForFirstConsumer',
        allowVolumeExpansion: true,
      }),
    ).toEqual({
      name: 'longhorn',
      provisioner: 'driver.longhorn.io',
      reclaimPolicy: 'Retain',
      bindingMode: 'WaitForFirstConsumer',
      allowsExpansion: true,
      isDefault: true,
    });
  });
});

describe('toClaimRow', () => {
  it('reads the phase, the provisioned size and the storage class', () => {
    expect(
      toClaimRow({
        spec: { storageClassName: 'longhorn', resources: { requests: { storage: '10Gi' } } },
        status: { phase: 'Bound', capacity: { storage: '10Gi' } },
      }),
    ).toEqual({ phase: 'Bound', capacity: '10Gi', storageClass: 'longhorn' });
  });

  it('shows no capacity until bound and a blank phase when none is reported', () => {
    expect(toClaimRow({ spec: { resources: { requests: { storage: '10Gi' } } } })).toEqual({
      phase: '',
      capacity: '—',
      storageClass: '—',
    });
  });
});

describe('toVolumeRow', () => {
  it('reads the declared size, reclaim policy, phase, bound claim and storage class', () => {
    expect(
      toVolumeRow({
        spec: {
          capacity: { storage: '50Gi' },
          persistentVolumeReclaimPolicy: 'Retain',
          claimRef: { namespace: 'payments', name: 'data' },
          storageClassName: 'longhorn',
        },
        status: { phase: 'Bound' },
      }),
    ).toEqual({
      capacity: '50Gi',
      reclaimPolicy: 'Retain',
      phase: 'Bound',
      claim: 'payments/data',
      storageClass: 'longhorn',
    });
  });

  it('marks what an unclaimed, bare volume lacks', () => {
    expect(toVolumeRow({})).toEqual({
      capacity: '—',
      reclaimPolicy: '—',
      phase: '',
      claim: '—',
      storageClass: '—',
    });
  });
});
