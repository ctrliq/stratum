import { describe, expect, it } from 'vitest';

import {
  averageOf,
  cniNameOf,
  defaultStorageClassNameOf,
  kubernetesVersionOf,
  regionOf,
  stratumVersionOf,
} from './selectors';

const toNamed = (name: string) => ({ metadata: { name } });

describe('kubernetesVersionOf', () => {
  it('drops the leading v', () => {
    expect(kubernetesVersionOf({ gitVersion: 'v1.37.1-stratum.0' })).toBe('1.37.1-stratum.0');
  });
});

describe('cniNameOf', () => {
  it.each([
    [['cilium', 'kube-proxy'], 'Cilium'],
    [['Cilium-Agent'], 'Cilium'],
    [['calico-node'], '—'],
    [[], '—'],
  ])('names the CNI of DaemonSets %j as %s', (names, cni) => {
    expect(cniNameOf(names.map(toNamed))).toBe(cni);
  });
});

describe('defaultStorageClassNameOf', () => {
  const longhorn = {
    metadata: {
      name: 'longhorn',
      annotations: { 'storageclass.kubernetes.io/is-default-class': 'true' },
    },
  };

  it('names the default storage class', () => {
    expect(defaultStorageClassNameOf([toNamed('local-path'), longhorn])).toBe('longhorn');
  });

  it('is a dash when no class is the default', () => {
    expect(defaultStorageClassNameOf([toNamed('local-path')])).toBe('—');
  });
});

describe('regionOf', () => {
  it.each([
    ['eu-west-1', ['eu-west-1a', 'eu-west-1b'], 'eu-west-1'],
    [undefined, ['—', 'eu-west-1b'], 'eu-west-1b'],
    ['—', ['eu-west-1a'], 'eu-west-1a'],
    ['', ['—', '—'], '—'],
    [undefined, [], '—'],
  ])('reads region %s with zones %o as %s', (region, zones, shown) => {
    expect(regionOf(region, zones)).toBe(shown);
  });
});

describe('stratumVersionOf', () => {
  it.each([
    ['2.1.0', ['v1.37.1-stratum.0-rc.1'], '2.1.0'],
    [undefined, ['v1.37.1-stratum.0-rc.1'], 'v1.37.1-stratum.0-rc.1'],
    ['—', ['v1.37.1', 'v1.37.1-stratum.0'], 'v1.37.1-stratum.0'],
    ['', ['v1.37.1', 'v1.37.1+k3s1'], '—'],
  ])('reads a recorded %s with kubelets %o as %s', (recorded, kubelets, shown) => {
    expect(stratumVersionOf(recorded, kubelets)).toBe(shown);
  });
});

describe('averageOf', () => {
  it('rounds the mean and reads nothing as zero', () => {
    expect(averageOf([20, 40, 75])).toBe(45);
    expect(averageOf([10, 50, 91])).toBe(50);
    expect(averageOf([])).toBe(0);
  });
});

describe('kubernetesVersionOf without a version', () => {
  it('reads an answer without a gitVersion as no version', () => {
    expect(kubernetesVersionOf({})).toBe('');
  });
});
