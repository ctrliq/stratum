import { describe, expect, it } from 'vitest';

import {
  fromArtifact,
  fromIdentity,
  fromIntent,
  fromReason,
  fromRefusal,
  fromSource,
  fromStatsSummary,
  fromVersion,
} from './parsers';

describe('fromVersion', () => {
  it.each([
    ['1.37.1', { major: '1', minor: '37', patch: '1' }],
    ['v1.37.1', { major: '1', minor: '37', patch: '1' }],
    ['v1.37.1-stratum.0', { major: '1', minor: '37', patch: '1' }],
    ['v1.37.1-stratum.0-rc.1', { major: '1', minor: '37', patch: '1' }],
    ['v1.37.1+k3s1', { major: '1', minor: '37', patch: '1' }],
    ['1.37', { major: '1', minor: '37', patch: '0' }],
  ])('reads %s', (text, version) => {
    expect(fromVersion(text)).toEqual(version);
  });

  it.each(['—', 'unknown', '', undefined])('reads %s as no version', (text) => {
    expect(fromVersion(text)).toBeUndefined();
  });
});

describe('fromStatsSummary', () => {
  it('reads the node usage out of a kubelet summary', () => {
    expect(
      fromStatsSummary({
        node: { cpu: { usageNanoCores: 5e8 }, memory: { workingSetBytes: 1024 } },
        pods: [],
      }),
    ).toEqual({ cpu: { usageNanoCores: 5e8 }, memory: { workingSetBytes: 1024 } });
  });

  it('leaves out an amount that is not a number', () => {
    expect(fromStatsSummary({ node: { cpu: { usageNanoCores: 'many' } } })).toEqual({
      cpu: { usageNanoCores: undefined },
      memory: { workingSetBytes: undefined },
    });
  });

  it.each([[undefined], ['not json'], [{}], [{ node: [] }]])('reads %o as no summary', (value) => {
    expect(fromStatsSummary(value)).toBeUndefined();
  });
});

describe('fromRefusal', () => {
  it('reads the code the hub refused with', () => {
    expect(fromRefusal({ status: 409, payload: { error: 'detail-expired' } })).toBe(
      'detail-expired',
    );
  });

  it.each([[new Error('offline')], [{ status: 500, payload: 'boom' }], [undefined]])(
    'reads %o as no code',
    (error) => {
      expect(fromRefusal(error)).toBeUndefined();
    },
  );
});

describe('version evidence', () => {
  it('shows a malformed value as unknown and keeps the well-formed ones beside it', () => {
    expect(fromIdentity({ raw: { nested: true }, availability: 'available' })).toEqual({
      raw: undefined,
      availability: 'available',
      digest: undefined,
      reason: undefined,
    });
  });

  it('keeps fields a newer hub added', () => {
    expect(fromIdentity({ raw: 'v1.36.4', signedBy: 'ciq' })).toMatchObject({
      raw: 'v1.36.4',
      signedBy: 'ciq',
    });
    expect(fromReason({ code: 'source-timeout', message: 'late', retryable: true })).toEqual({
      code: 'source-timeout',
      message: 'late',
      retryable: true,
    });
  });

  it('checks replica instances one by one', () => {
    expect(
      fromIdentity({ raw: 'v1.36.4', instances: [{ raw: 'v1.36.4' }, 'broken', { raw: 7 }] })
        ?.instances,
    ).toEqual([
      { raw: 'v1.36.4', availability: undefined, digest: undefined, reason: undefined },
      { raw: undefined, availability: undefined, digest: undefined, reason: undefined },
    ]);
    expect(fromIdentity({ raw: 'v1.36.4', instances: 'broken' })?.instances).toEqual([]);
  });

  it.each([[undefined], [null], ['text'], [[1]]])('reads %o as no evidence', (value) => {
    expect(fromIdentity(value)).toBeUndefined();
    expect(fromReason(value)).toBeUndefined();
    expect(fromArtifact(value)).toBeUndefined();
    expect(fromIntent(value)).toBeUndefined();
    expect(fromSource(value)).toBeUndefined();
  });

  it('reads a rollout intent and its target release', () => {
    expect(
      fromIntent({ phase: 'running', action: 7, targetRelease: { releaseID: 'r-42' } }),
    ).toMatchObject({
      phase: 'running',
      action: undefined,
      targetRelease: { releaseID: 'r-42', digest: undefined },
    });
  });

  it('reads the writer identity as text only', () => {
    expect(fromSource({ producer: 'agent', build: 3 })).toMatchObject({
      producer: 'agent',
      build: undefined,
    });
  });
});
