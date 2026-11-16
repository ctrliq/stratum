import { describe, expect, it } from 'vitest';

import type { SigningRequest } from '../types';
import {
  hasEvidence,
  isApprovedRequest,
  isAttemptFailed,
  isEvidenceOpen,
  isEvidencePage,
  isKubeletServingRequest,
  isLaterAttempt,
  isLaterSuccess,
  isPendingKnown,
  isPendingRequest,
  isReadStanding,
  isReportDetail,
  isRestrictedNamespace,
  isSameEvidence,
  isSameFilters,
  isShownRaw,
} from './predicates';

const toRequest = (status: SigningRequest['status']): SigningRequest => ({
  spec: { signerName: 'kubernetes.io/kubelet-serving' },
  status,
});

const toCondition = (type: string) => ({ type, status: 'True' });

describe('isKubeletServingRequest', () => {
  it.each([
    ['kubernetes.io/kubelet-serving', true],
    ['kubernetes.io/kube-apiserver-client', false],
  ])('reads a request for the signer %s as kubelet-serving: %s', (signerName, expected) => {
    expect(isKubeletServingRequest({ spec: { signerName } })).toBe(expected);
  });
});

describe('isApprovedRequest and isPendingRequest', () => {
  it.each<[string, SigningRequest['status'], boolean, boolean]>([
    ['an Approved condition', { conditions: [toCondition('Approved')] }, true, false],
    ['a Denied condition', { conditions: [toCondition('Denied')] }, false, false],
    ['a Failed condition', { conditions: [toCondition('Failed')] }, false, false],
    ['no condition but an issued certificate', { certificate: 'LS0tLS1CRUdJTg==' }, false, false],
    ['no condition and no certificate', {}, false, true],
    ['no status', undefined, false, true],
  ])('reads a request with %s', (_state, status, isApproved, isPending) => {
    expect(isApprovedRequest(toRequest(status))).toBe(isApproved);
    expect(isPendingRequest(toRequest(status))).toBe(isPending);
  });
});

describe('isRestrictedNamespace', () => {
  it.each([
    [{ 'pod-security.kubernetes.io/enforce': 'restricted' }, true],
    [{ 'pod-security.kubernetes.io/enforce': 'baseline' }, false],
    [{}, false],
  ])('reads labels %o as restricted: %s', (labels, expected) => {
    expect(isRestrictedNamespace({ metadata: { labels } })).toBe(expected);
  });
});

const now = Date.UTC(2026, 0, 10);
const at = (secondsFromNow: number): string => new Date(now + secondsFromNow * 1000).toISOString();

describe('isPendingKnown', () => {
  it.each([
    [{ pending: 0, availability: 'available' }, true],
    [{ pending: undefined, availability: 'available' }, false],
    [{ pending: 2, availability: 'forbidden' }, false],
    [{ pending: undefined, availability: 'unknown' }, false],
  ])('reads %o as %s', (pending, expected) => {
    expect(isPendingKnown(pending)).toBe(expected);
  });
});

describe('isReportDetail and isEvidencePage', () => {
  it('accept only a body that carries its list', () => {
    expect([
      isReportDetail({ findings: [] }),
      isReportDetail({ items: [] }),
      isReportDetail('text'),
      isEvidencePage({ items: [] }),
      isEvidencePage({ findings: [] }),
    ]).toEqual([true, false, false, true, false]);
  });
});

describe('isSameFilters', () => {
  it('compares the three filters', () => {
    const filters = { signal: 'certificate', nodeUID: '', minimumSeverity: '' };
    expect([
      isSameFilters(filters, { ...filters }),
      isSameFilters(filters, { ...filters, minimumSeverity: 'warning' }),
    ]).toEqual([true, false]);
  });
});

describe('hasEvidence and isEvidenceOpen', () => {
  it.each([
    [{ evidenceRef: 'ev-1' }, true],
    [{ evidenceRef: null }, false],
    [{}, false],
  ])('offer evidence for %o: %s', (finding, expected) => {
    expect(hasEvidence(finding)).toBe(expected);
  });

  it('marks every finding that shares the open reference', () => {
    const opened = { evidenceRef: 'ev-1' };
    expect([
      isEvidenceOpen({ evidenceRef: 'ev-1', id: 'a' }, opened),
      isEvidenceOpen({ evidenceRef: 'ev-1', id: 'b' }, opened),
      isEvidenceOpen({ evidenceRef: 'ev-2' }, opened),
      isEvidenceOpen({ evidenceRef: 'ev-1' }, undefined),
    ]).toEqual([true, true, false, false]);
  });
});

describe('isShownRaw', () => {
  it.each([
    ['version', 'version', true],
    [undefined, 'version', true],
    ['supervisor', 'supervisor', false],
    ['certificate', 'certificate', false],
    ['certificate', 'backup', true],
    ['backup', 'backup', false],
    ['backup', 'certificate', true],
    ['version', 'certificate', true],
  ])('an item of type %s under a %s finding is raw: %s', (type, signal, expected) => {
    expect(isShownRaw({ type }, signal)).toBe(expected);
  });
});

describe('isAttemptFailed', () => {
  it.each([
    [{ outcome: 'failed', completedAt: at(-60), lastSuccessAt: at(-600) }, true],
    [{ outcome: 'success', completedAt: at(-60) }, true],
    [{ outcome: 'success', completedAt: at(-60), lastSuccessAt: at(-60) }, false],
    [{ outcome: 'failed' }, false],
    [undefined, false],
  ])('reads %o as failed: %s', (attempt, expected) => {
    expect(isAttemptFailed(attempt)).toBe(expected);
  });
});

describe('isLaterAttempt', () => {
  const done = {
    attemptedAt: at(-60),
    completedAt: at(-50),
    outcome: 'success',
    lastSuccessAt: at(-50),
  };
  it.each([
    ['a later attempt time', { attemptedAt: at(-10) }, { attemptedAt: at(-60) }, true],
    ['a stated attempt time over none', { attemptedAt: at(-60) }, {}, true],
    ['completion over none', { completedAt: at(-50) }, {}, true],
    ['the later completion', { completedAt: at(-10) }, { completedAt: at(-50) }, true],
    ['a failure at the same instant', { ...done, outcome: 'failed' }, done, true],
    [
      'the alphabetically smaller outcome word',
      { outcome: 'aborted' },
      { outcome: 'skipped' },
      true,
    ],
    ['nothing when both are the same', done, done, false],
  ])('prefers %s', (_case, first, second, expected) => {
    expect([isLaterAttempt(first, second), isLaterAttempt(second, first)]).toEqual([
      expected,
      false,
    ]);
  });
});

describe('isLaterSuccess', () => {
  it.each([
    [now - 1000, undefined, true],
    [undefined, now - 1000, false],
    [now - 1000, now + 60_000, true],
    [now + 60_000, now - 1000, false],
    [now - 1000, now - 5000, true],
    [now + 90_000, now + 60_000, true],
  ])('keeps %s over %s: %s', (first, second, expected) => {
    expect(isLaterSuccess(first, second, now)).toBe(expected);
  });
});

describe('isReadStanding', () => {
  it.each([
    [{ availability: 'available' }, true],
    [{}, true],
    [undefined, true],
    [{ availability: 'forbidden' }, false],
    [{ availability: 'unavailable' }, false],
  ])('reads %o as standing: %s', (observation, expected) => {
    expect(isReadStanding(observation)).toBe(expected);
  });
});

describe('isSameEvidence', () => {
  it('compares the reference and the report it was read for', () => {
    const opened = { evidenceRef: 'ev-1', reportGeneration: 'g-7' };
    expect([
      isSameEvidence(opened, { ...opened }),
      isSameEvidence(opened, { ...opened, reportGeneration: 'g-8' }),
      isSameEvidence(opened, { ...opened, evidenceRef: 'ev-2' }),
      isSameEvidence(undefined, opened),
    ]).toEqual([true, false, false, false]);
  });
});
