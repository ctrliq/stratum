import { describe, expect, it } from 'vitest';

import { deployment, registration } from '../testing/fixtures';
import { toDeployRequest, toKubeconfigRequest, toNodeCount, toTokenRequest } from './transforms';

describe('toTokenRequest', () => {
  it('asks for the name, profile and lifetime, without a blank display name', () => {
    expect(toTokenRequest(registration)).toEqual({
      clusterName: 'staging',
      profile: 'full',
      ttlSeconds: 3600,
    });
  });

  it.each([
    ['3600', 3600],
    ['14400', 14400],
    ['86400', 86400],
  ] as const)('sends the lifetime %s as %s seconds', (lifetime, seconds) => {
    expect(toTokenRequest({ ...registration, lifetime }).ttlSeconds).toBe(seconds);
  });

  it('sends the read-only profile as readonly and the display name trimmed', () => {
    expect(
      toTokenRequest({ ...registration, profile: 'readonly', displayName: '  Staging EU  ' }),
    ).toMatchObject({ profile: 'readonly', displayName: 'Staging EU' });
  });
});

describe('toKubeconfigRequest', () => {
  it('sends the name, the file and the optional fields', () => {
    expect(
      toKubeconfigRequest({
        ...registration,
        name: 'edge',
        kubeconfig: 'apiVersion: v1',
        displayName: 'Edge',
        context: 'prod-ctx',
      }),
    ).toEqual({
      name: 'edge',
      kubeconfig: 'apiVersion: v1',
      displayName: 'Edge',
      context: 'prod-ctx',
    });
  });

  it('leaves blank optional fields out', () => {
    expect(
      toKubeconfigRequest({ ...registration, kubeconfig: 'x', displayName: ' ', context: ' ' }),
    ).toEqual({ name: 'staging', kubeconfig: 'x' });
  });
});

describe('toDeployRequest', () => {
  it('sends the full specification', () => {
    expect(
      toDeployRequest({
        ...deployment,
        version: ' v1.34.0 ',
        displayName: 'Staging EU',
        placement: 'bridge=vmbr0',
      }),
    ).toEqual({
      name: 'staging-eu',
      provider: 'proxmox',
      kubernetesVersion: 'v1.34.0',
      controlPlane: 3,
      workers: 5,
      displayName: 'Staging EU',
      values: { bridge: 'vmbr0' },
    });
  });

  it('leaves a blank display name and empty placement values out', () => {
    expect(toDeployRequest(deployment)).toEqual({
      name: 'staging-eu',
      provider: 'proxmox',
      kubernetesVersion: 'v1.34.0',
      controlPlane: 3,
      workers: 5,
    });
  });
});

describe('toNodeCount', () => {
  it.each([
    [0, 1],
    [3, 3],
    [2.6, 3],
    [9, 7],
  ])('keeps %s within 1 to 7 as the whole number %s', (value, count) => {
    expect(toNodeCount(value, { min: 1, max: 7 })).toBe(count);
  });
});
