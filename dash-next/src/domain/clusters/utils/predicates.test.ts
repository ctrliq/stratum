import { describe, expect, it } from 'vitest';

import { deployment, registration, toCluster, toRefusal } from '../testing/fixtures';
import {
  hasErrorCode,
  isCapabilityAvailable,
  isClusterName,
  isDeployReady,
  isDestroyable,
  isDestroyConfirmed,
  isHealthy,
  isKubernetesVersion,
  isNameTaken,
  isProvisioningDisabled,
  isRegistrationReady,
  isRetryable,
  isUsable,
} from './predicates';

const fleet = [toCluster({ id: 'c-1', name: 'prod' })];

describe('isHealthy and isUsable', () => {
  it.each([
    ['healthy', true, true],
    ['degraded', false, true],
    ['disconnected', false, false],
    ['pending', false, true],
    ['provisioning', false, true],
  ] as const)('status %s is healthy %s and usable %s', (status, healthy, usable) => {
    expect([isHealthy({ status }), isUsable({ status })]).toEqual([healthy, usable]);
  });
});

describe('isClusterName', () => {
  it.each([
    ['staging', true],
    ['a', true],
    ['staging-2', true],
    ['', false],
    ['Staging', false],
    ['-staging', false],
    ['staging-', false],
    ['stag_ing', false],
    ['stag.ing', false],
    ['a'.repeat(64), false],
  ])('%s is a cluster name: %s', (name, verdict) => {
    expect(isClusterName(name)).toBe(verdict);
  });
});

describe('isNameTaken', () => {
  it('is taken by the name or the id of a listed cluster', () => {
    expect(['prod', 'c-1', 'staging', ''].map((name) => isNameTaken(fleet, name))).toEqual([
      true,
      true,
      false,
      false,
    ]);
  });
});

describe('isKubernetesVersion', () => {
  it.each([
    ['v1.34.0', true],
    ['v1.34.0-rc.1', true],
    ['v10.2.33', true],
    [' v1.34.0 ', true],
    ['1.34.0', false],
    ['v1.34', false],
    ['v1.34.0+build', false],
    ['latest', false],
    ['', false],
  ])('%s is accepted: %s', (version, verdict) => {
    expect(isKubernetesVersion(version)).toBe(verdict);
  });
});

describe('isRegistrationReady', () => {
  it('needs a free, valid name for a token', () => {
    expect(
      ['staging', '', 'Staging', 'prod'].map((name) =>
        isRegistrationReady({ ...registration, name }, fleet),
      ),
    ).toEqual([true, false, false, false]);
  });

  it.each([
    ['staging', 'apiVersion: v1', true],
    ['staging', '', false],
    ['staging', '  \n ', false],
    ['Staging', 'apiVersion: v1', false],
  ])('by kubeconfig with name %s and text %j is ready: %s', (name, kubeconfig, verdict) => {
    expect(
      isRegistrationReady({ ...registration, method: 'kubeconfig', name, kubeconfig }, fleet),
    ).toBe(verdict);
  });
});

describe('isDeployReady', () => {
  it.each([
    [{}, true],
    [{ name: '' }, false],
    [{ name: 'Staging' }, false],
    [{ name: 'prod' }, false],
    [{ provider: '' }, false],
    [{ version: 'latest' }, false],
    [{ version: '' }, false],
    [{ placement: 'bridge' }, false],
  ])('with %j is ready: %s', (changed, verdict) => {
    expect(isDeployReady({ ...deployment, ...changed }, fleet)).toBe(verdict);
  });
});

describe('isDestroyable', () => {
  it.each([
    [false, true, false, true],
    [false, true, true, false],
    [false, false, false, false],
    [true, true, false, false],
  ])(
    'fleet manager %s, built %s, protected %s: %s',
    (fleetManager, provisioned, deletionProtected, verdict) => {
      expect(isDestroyable({ fleetManager, provisioned, deletionProtected })).toBe(verdict);
    },
  );
});

describe('isDestroyConfirmed', () => {
  it.each([
    ['', false],
    ['la', false],
    ['Lab', false],
    ['LAB', false],
    ['lab', true],
    [' lab ', true],
    ['lab2', false],
  ])('typing %j confirms: %s', (typed, verdict) => {
    expect(isDestroyConfirmed(typed, { id: 'lab' })).toBe(verdict);
  });
});

describe('refusal predicates', () => {
  it('reads the error code of a refusal', () => {
    expect([
      hasErrorCode(toRefusal(409, 'name-taken', 'taken'), 'name-taken'),
      hasErrorCode(toRefusal(409, 'other', 'taken'), 'name-taken'),
      hasErrorCode(new Error('boom'), 'name-taken'),
    ]).toEqual([true, false, false]);
  });

  it('knows a hub without provisioning by status 501 or by its code', () => {
    expect(
      [
        toRefusal(501, 'not-implemented', 'no'),
        toRefusal(400, 'capi-not-enabled', 'no'),
        toRefusal(403, 'insufficient-role', 'admin only'),
      ].map(isProvisioningDisabled),
    ).toEqual([true, true, false]);
  });

  it('is retryable only when the hub says so', () => {
    expect(
      [
        { status: 503, payload: { error: 'busy', message: 'hub overloaded', retryable: true } },
        { status: 404, payload: { error: 'not-found', message: 'no cluster edge' } },
      ].map(isRetryable),
    ).toEqual([true, false]);
  });
});

describe('isCapabilityAvailable', () => {
  it('reads only the exact state available as available', () => {
    expect(
      [{ state: 'available' as const }, { state: 'unavailable' as const }, {}].map(
        isCapabilityAvailable,
      ),
    ).toEqual([true, false, false]);
  });
});
