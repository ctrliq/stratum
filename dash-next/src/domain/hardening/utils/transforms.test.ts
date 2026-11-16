import { describe, expect, it } from 'vitest';

import { backupReadings } from '../constants';
import type { Posture, PostureReads, SigningRequest } from '../types';
import type { OpenedEvidence } from '../types/engineState';
import type { BackupEvidence } from '../types/evidence';
import { approvedCountOf } from './selectors';
import {
  fromLease,
  fromLeaseError,
  fromLeaseFact,
  fromPendingRequestsFact,
  fromRequestListError,
  toBackupReading,
  toCertificateCauses,
  toCertificateRows,
  toControlGroups,
  toDistinctBackups,
  toFindingsQuery,
  toOptions,
  toPendingRequests,
  toPosture,
  toPostureCards,
  toPostureLoad,
  toPostureSignals,
  toReconciledAttempt,
  toRefusal,
  toRestrictedNamespaces,
  toRoleCoverage,
  toWalkedCertificateRows,
  toWalkedRoleCoverage,
} from './transforms';

const toApproved = (): SigningRequest => ({
  spec: { signerName: 'kubernetes.io/kubelet-serving' },
  status: { conditions: [{ type: 'Approved', status: 'True' }] },
});

const toNamespace = (name: string, labels: Record<string, string> = {}) => ({
  metadata: { name, labels },
});

describe('toPendingRequests', () => {
  it('reads an answer with no requests as an observed zero', () => {
    expect(toPendingRequests([])).toEqual({ pending: 0, availability: 'available' });
  });

  it('counts the undecided kubelet-serving requests', () => {
    expect(
      toPendingRequests([toApproved(), { spec: { signerName: 'kubernetes.io/kubelet-serving' } }]),
    ).toEqual({ pending: 1, availability: 'available' });
  });
});

describe('fromRequestListError', () => {
  it.each<[number | 'unknown', string]>([
    [403, 'forbidden'],
    [500, 'unknown'],
    ['unknown', 'unknown'],
  ])('reads a list ending with %s as %s with no pending count', (status, availability) => {
    expect(fromRequestListError({ status, payload: undefined })).toEqual({
      pending: undefined,
      availability,
    });
  });
});

describe('fromPendingRequestsFact', () => {
  it('shows the hub’s pending count beside the cluster’s own approved count', () => {
    const approved = approvedCountOf(Array.from({ length: 5 }, toApproved));
    const { pending } = fromPendingRequestsFact({
      kubeletServingCSRPending: 2,
      kubeletServingCSRAvailability: 'available',
    });
    expect({ approved, pending }).toEqual({ approved: 5, pending: 2 });
  });

  it('carries the hub’s availability', () => {
    expect(
      fromPendingRequestsFact({
        kubeletServingCSRPending: null,
        kubeletServingCSRAvailability: 'forbidden',
      }),
    ).toEqual({ pending: undefined, availability: 'forbidden' });
  });

  it('leaves the availability unknown when the hub states none', () => {
    expect(fromPendingRequestsFact({ kubeletServingCSRPending: 2 }).availability).toBe('unknown');
  });
});

describe('toRestrictedNamespaces', () => {
  it('counts and names the namespaces enforcing the restricted level', () => {
    expect(
      toRestrictedNamespaces([
        toNamespace('default'),
        toNamespace('kube-system'),
        toNamespace('shop', { 'pod-security.kubernetes.io/enforce': 'restricted' }),
        toNamespace('blog', { 'pod-security.kubernetes.io/enforce': 'baseline' }),
        toNamespace('pay', { 'pod-security.kubernetes.io/enforce': 'restricted' }),
        toNamespace('ops'),
      ]),
    ).toEqual({ names: ['shop', 'pay'], restrictedCount: 2, namespaceCount: 6 });
  });

  it('leaves the counts unknown when the namespace list could not be read', () => {
    expect(toRestrictedNamespaces(undefined)).toEqual({
      names: undefined,
      restrictedCount: undefined,
      namespaceCount: undefined,
    });
  });
});

const now = Date.UTC(2026, 0, 10, 12);
const at = (secondsFromNow: number): string => new Date(now + secondsFromNow * 1000).toISOString();

describe('fromLeaseFact', () => {
  it.each([
    [{ state: 'active', holder: 'operator-0' }, 'active', 'operator-0'],
    [{ state: 'inactive', holder: '' }, 'inactive', '—'],
    [{ state: 'missing', holder: 'operator-0' }, 'missing', '—'],
    [{ state: 'draining', holder: 'operator-0' }, 'draining', 'operator-0'],
    [undefined, 'unknown', '—'],
  ])('takes the hub verdict %o as %s held by %s', (lease, state, holder) => {
    expect(fromLeaseFact(lease)).toEqual({ state, holder, isRefused: false });
  });
});

describe('fromLease', () => {
  it.each([
    [{ holderIdentity: 'op', renewTime: at(-100), leaseDurationSeconds: 60 }, 'active', 'op'],
    [{ holderIdentity: 'op', renewTime: at(-50) }, 'active', 'op'],
    [{ holderIdentity: 'op', renewTime: at(-70) }, 'inactive', 'op'],
    [{ holderIdentity: 'op', renewTime: at(31) }, 'unknown', 'op'],
    [{ renewTime: at(-5) }, 'inactive', '—'],
    [{ holderIdentity: 'op' }, 'inactive', 'op'],
  ])('derives the verdict of the lease %o as %s held by %s', (spec, state, holder) => {
    expect(fromLease({ spec }, now)).toEqual({ state, holder, isRefused: false });
  });
});

describe('fromLeaseError', () => {
  it.each<[number | 'unknown', string, boolean]>([
    [404, 'missing', false],
    [403, 'unknown', true],
    [500, 'unknown', false],
    ['unknown', 'unknown', false],
  ])('reads a lease read ending with %s as %s (refused: %s)', (status, state, isRefused) => {
    expect(fromLeaseError({ status, payload: undefined })).toEqual({
      state,
      holder: '—',
      isRefused,
    });
  });
});

const posture: Posture = {
  operator: { state: 'active', holder: 'operator-0', isRefused: false },
  approved: 5,
  pending: { pending: 0, availability: 'available' },
  restricted: { names: ['prod', 'payments'], restrictedCount: 2, namespaceCount: 12 },
  controlPlaneCount: 3,
  observedAt: undefined,
};

describe('toPostureCards', () => {
  const cardsOf = (patch: Partial<Posture>) =>
    toPostureCards({ ...posture, ...patch }).map(({ value, caption, isWarning }) => [
      value,
      caption,
      isWarning,
    ]);

  it('states the four cards', () => {
    expect(cardsOf({})).toEqual([
      ['Active', 'operator-0', false],
      ['2 / 12 ns', 'enforce=restricted', false],
      ['5 approved', '0 pending', false],
      ['3 nodes', 'Stratum control-plane label', false],
    ]);
  });

  it.each([
    [{ names: [], restrictedCount: 0, namespaceCount: 12 }, '0 / 12 ns', 'not enforced'],
    [
      { names: undefined, restrictedCount: undefined, namespaceCount: undefined },
      'unknown',
      'namespace read unavailable',
    ],
  ])('reads the namespaces %o as %s, %s', (restricted, value, caption) => {
    expect(cardsOf({ restricted })[1]).toEqual([value, caption, false]);
  });

  it.each([
    [5, { pending: 2, availability: 'available' }, ['5 approved', '2 pending', true]],
    [
      5,
      { pending: undefined, availability: 'unknown' },
      ['5 approved', 'CSR pending count unknown', false],
    ],
    [
      5,
      { pending: undefined, availability: 'available' },
      ['5 approved', 'CSR pending count unknown', false],
    ],
    [undefined, { pending: 2, availability: 'available' }, ['unknown approved', '2 pending', true]],
  ])('reads %s approved with %o as %o', (approved, pending, card) => {
    expect(cardsOf({ approved, pending })[2]).toEqual(card);
  });
});

describe('toPostureSignals', () => {
  const signalsOf = (patch: Partial<Posture>) => toPostureSignals({ ...posture, ...patch });

  it('marks every signal ok on a hardened cluster', () => {
    expect(signalsOf({})).toEqual([
      { label: 'Operator / CSR approver running', isOk: true, detail: 'operator lease held' },
      {
        label: 'No kubelet-serving CSR stuck pending',
        isOk: true,
        detail: '5 approved · 0 pending (kubelet-serving signer)',
      },
      { label: "PodSecurity 'restricted' enforced", isOk: true, detail: 'prod, payments' },
      {
        label: 'Control-plane nodes labeled',
        isOk: true,
        detail: '3 node(s) carrying the Stratum control-plane label',
      },
    ]);
  });

  it.each([
    [
      { pending: 2, availability: 'available' },
      'No kubelet-serving CSR stuck pending',
      '5 approved · 2 pending (kubelet-serving signer)',
    ],
    [
      { pending: undefined, availability: 'forbidden' },
      'Kubelet-serving CSR pending state unknown',
      'CSR pending count unknown — the cluster credential cannot list requests',
    ],
    [
      { pending: undefined, availability: 'unknown' },
      'Kubelet-serving CSR pending state unknown',
      'CSR pending count unknown — no usable observation',
    ],
    [
      { pending: undefined, availability: 'available' },
      'Kubelet-serving CSR pending state unknown',
      'CSR pending count unknown — no usable observation',
    ],
  ])('warns about the pending requests %o', (pending, label, detail) => {
    expect(signalsOf({ pending })[1]).toEqual({ label, isOk: false, detail });
  });

  it('states an unknown approved count beside the pending count the hub states', () => {
    expect(
      signalsOf({ approved: undefined, pending: { pending: 2, availability: 'available' } })[1]
        .detail,
    ).toBe('unknown approved · 2 pending (kubelet-serving signer)');
  });

  it('warns when no namespace enforces the restricted level or the read was unavailable', () => {
    expect([
      signalsOf({ restricted: { names: [], restrictedCount: 0, namespaceCount: 12 } })[2],
      signalsOf({
        restricted: { names: undefined, restrictedCount: undefined, namespaceCount: undefined },
      })[2],
    ]).toEqual([
      {
        label: "PodSecurity 'restricted' enforced",
        isOk: false,
        detail: "no namespace enforces PodSecurity 'restricted'",
      },
      {
        label: "PodSecurity 'restricted' state unknown",
        isOk: false,
        detail: 'PodSecurity restricted count unknown — the namespace read was unavailable',
      },
    ]);
  });

  it('warns about an inactive operator and an unlabelled control plane', () => {
    const signals = signalsOf({
      operator: { state: 'inactive', holder: '—', isRefused: false },
      controlPlaneCount: 0,
    });
    expect([signals[0].isOk, signals[3]]).toEqual([
      false,
      {
        label: 'Control-plane nodes labeled',
        isOk: false,
        detail: '0 node(s) carrying the Stratum control-plane label',
      },
    ]);
  });
});

describe('toControlGroups', () => {
  const toControl = (id: string, group: string, source: string) => ({
    id,
    desc: id,
    group,
    source,
  });

  it('groups controls in the order the hub lists them, naming each source once', () => {
    const first = toControl('1.2.1', 'API Server', 'kube-apiserver.yaml');
    const second = toControl('4.2.1', 'Kubelet', 'kubelet.conf');
    const third = toControl('1.2.9', 'API Server', 'admission-webhook.yaml');
    const fourth = toControl('1.2.10', 'API Server', 'kube-apiserver.yaml');
    expect(toControlGroups([first, second, third, fourth])).toEqual([
      {
        group: 'API Server',
        sources: ['kube-apiserver.yaml', 'admission-webhook.yaml'],
        controls: [first, third, fourth],
      },
      { group: 'Kubelet', sources: ['kubelet.conf'], controls: [second] },
    ]);
  });
});

describe('toRefusal', () => {
  it.each([
    [{ status: 'unknown', payload: 'Failed to fetch' }, 'Failed to fetch', false],
    [{ status: 'unknown', payload: '' }, 'the engine-state request failed', false],
    [
      { status: 'unknown', payload: 'signal timed out', reason: 'timeout' },
      'the engine-state request timed out',
      true,
    ],
    [
      {
        status: 503,
        payload: { error: 'unavailable', message: 'hub overloaded', retryable: true },
      },
      'hub overloaded',
      true,
    ],
    [
      {
        status: 503,
        payload: { error: 'unavailable', message: 'hub overloaded', retryable: false },
      },
      'hub overloaded',
      false,
    ],
    [
      { status: 503, payload: { error: 'unavailable', message: 'hub overloaded' } },
      'hub overloaded',
      false,
    ],
    [
      { status: 409, payload: { error: 'generation-changed', retryable: true } },
      'generation-changed',
      true,
    ],
    [{ status: 409, payload: { error: 'generation-changed' } }, 'generation-changed', false],
    [{ status: 502, payload: ' bad gateway ' }, 'bad gateway', true],
    [{ status: 429, payload: 'slow down' }, 'slow down', true],
    [{ status: 404, payload: 'not here' }, 'not here', false],
    [{ status: 503, payload: undefined }, 'HTTP 503', true],
    [{ status: 403, payload: undefined }, 'HTTP 403', false],
    [{ status: 503, payload: '' }, 'HTTP 503', true],
    [{ status: 403, payload: '' }, 'HTTP 403', false],
    [
      { status: 200, payload: 'Unexpected end of JSON input', reason: 'unreadable' },
      'the engine-state response was not a JSON object',
      false,
    ],
    [
      { status: 200, payload: 'signal timed out', reason: 'timeout' },
      'the engine-state request timed out',
      true,
    ],
    [
      { status: 200, payload: { message: 'the engine-state response was not a JSON object' } },
      'the engine-state response was not a JSON object',
      false,
    ],
  ])('turns %o into "%s" (retryable: %s)', (error, message, isRetryable) => {
    expect(toRefusal(error)).toMatchObject({ status: error.status, message, isRetryable });
  });

  it('keeps the facts the hub attached and leaves the others absent', () => {
    expect([
      toRefusal({
        status: 409,
        payload: {
          error: 'generation-changed',
          message: 'moved',
          requestID: 'req-1',
          retryable: false,
          currentReportGeneration: 'g-8',
        },
      }),
      toRefusal({ status: 410, payload: { error: 'detail-expired', message: 'expired' } }),
      toRefusal({
        status: 410,
        payload: { error: 'detail-expired', message: 'expired' },
        headers: {
          'x-stratum-retained-from': '2026-01-01T00:00:00Z',
          'x-stratum-retained-to': '2026-01-08T00:00:00Z',
        },
      }),
    ]).toEqual([
      {
        status: 409,
        message: 'moved',
        isRetryable: false,
        code: 'generation-changed',
        requestId: 'req-1',
        currentReportGeneration: 'g-8',
      },
      { status: 410, message: 'expired', isRetryable: false, code: 'detail-expired' },
      {
        status: 410,
        message: 'expired',
        isRetryable: false,
        code: 'detail-expired',
        retainedFrom: '2026-01-01T00:00:00Z',
        retainedTo: '2026-01-08T00:00:00Z',
      },
    ]);
  });
});

describe('toFindingsQuery', () => {
  it('sends only the selected filters, beside the cursor', () => {
    const filters = { signal: 'certificate', nodeUID: 'uid-1', minimumSeverity: 'warning' };
    expect([
      toFindingsQuery({ signal: '', nodeUID: '', minimumSeverity: '' }),
      toFindingsQuery({ signal: '', nodeUID: 'uid-1', minimumSeverity: '' }),
      toFindingsQuery(filters, 'c1'),
    ]).toEqual([{}, { nodeUID: 'uid-1' }, { ...filters, cursor: 'c1' }]);
  });
});

describe('toOptions', () => {
  it('offers the all choice first', () => {
    expect(toOptions(['certificate', 'version'], 'All signals')).toEqual([
      ['All signals', ''],
      ['certificate', 'certificate'],
      ['version', 'version'],
    ]);
  });
});

const subject = { scope: 'node', nodeUID: 'uid-1', component: 'kubelet', key: 'serving' };
const toCoverage = (
  expected: number,
  evaluated: number,
  unknown: number,
  notApplicable: number,
) => ({
  expected,
  evaluated,
  unknown,
  notApplicable,
});

describe('toRoleCoverage', () => {
  it('sums the coverage per role, in the order each role first appears', () => {
    expect(
      toRoleCoverage([
        {
          observation: { coverage: toCoverage(2, 1, 1, 0) },
          evidence: { role: 'kubelet-serving' },
        },
        { observation: { coverage: toCoverage(1, 1, 0, 0) }, evidence: { role: 'apiserver' } },
        {
          observation: { coverage: toCoverage(1, 1, 0, 3) },
          evidence: { role: 'kubelet-serving' },
        },
      ]),
    ).toEqual([
      { role: 'kubelet-serving', ...toCoverage(3, 2, 1, 3) },
      { role: 'apiserver', ...toCoverage(1, 1, 0, 0) },
    ]);
  });

  it('tolerates incomplete observations', () => {
    expect(
      toRoleCoverage([
        { observation: { coverage: toCoverage(1, 1, 0, 0) }, evidence: {} },
        { observation: { coverage: toCoverage(1, 1, 0, 0) }, evidence: undefined },
        { observation: {}, evidence: { role: 'etcd' } },
        {
          observation: { coverage: { expected: Number.NaN, evaluated: 1 } },
          evidence: { role: 'etcd' },
        },
      ]),
    ).toEqual([{ role: 'etcd', ...toCoverage(0, 1, 0, 0) }]);
  });
});

describe('toCertificateRows', () => {
  const walkedFirst = { signal: 'certificate', subject, id: 'o-1', evidence: null };
  const walkedSecond = {
    signal: 'certificate',
    subject: { ...subject, nodeUID: 'uid-2' },
    id: 'o-2',
    evidence: { type: 'certificate', role: 'apiserver' },
  };
  const opened: OpenedEvidence = {
    evidenceRef: 'ev-1',
    reportGeneration: 'g-7',
    signal: 'certificate',
    severity: 'warning',
    observation: walkedFirst,
    certificateObservations: [walkedFirst, walkedSecond],
  };
  const served = { type: 'certificate', certificateID: 'c-1' };
  const extra = { type: 'certificate', certificateID: 'c-2' };

  it('fills the opened observation with the served fragment and appends the extra ones', () => {
    expect(toCertificateRows(opened, [served, { type: 'version' }, extra])).toEqual([
      { observation: walkedFirst, evidence: served },
      { observation: walkedSecond, evidence: walkedSecond.evidence },
      { observation: undefined, evidence: extra },
    ]);
  });

  it('appends every fragment when no walked observation is the opened one', () => {
    expect(toCertificateRows({ ...opened, observation: undefined }, [served])).toEqual([
      { observation: walkedFirst, evidence: undefined },
      { observation: walkedSecond, evidence: walkedSecond.evidence },
      { observation: undefined, evidence: served },
    ]);
  });

  it('has no rows without observations and fragments, or under another signal', () => {
    expect([
      toCertificateRows({ ...opened, observation: undefined, certificateObservations: [] }, []),
      toCertificateRows({ ...opened, signal: 'backup' }, [served]),
    ]).toEqual([[], []]);
  });

  it('has a role line, without roles, once a certificate observation without a role was walked', () => {
    expect(toWalkedRoleCoverage([walkedFirst])).toEqual([]);
  });

  it('has no role line when no certificate observation was walked', () => {
    expect(toWalkedRoleCoverage([])).toBeUndefined();
  });

  it('reads walked observations with their own evidence', () => {
    expect(toWalkedCertificateRows([walkedFirst, walkedSecond])).toEqual([
      { observation: walkedFirst, evidence: undefined },
      { observation: walkedSecond, evidence: walkedSecond.evidence },
    ]);
  });
});

describe('toCertificateCauses', () => {
  it.each([
    ['readable', 'available', 'success', []],
    ['readable', 'available', undefined, []],
    ['corrupt', 'available', 'success', ['read state corrupt']],
    ['readable', 'forbidden', 'success', ['availability forbidden']],
    ['unreadable', 'unavailable', 'success', ['read state unreadable · availability unavailable']],
    ['readable', 'available', 'failed', ['renewal failed']],
    [
      'missing',
      'forbidden',
      'failed',
      ['read state missing · availability forbidden', 'renewal failed'],
    ],
    ['quarantined', 'available', 'success', ['read state quarantined']],
  ])('reads %s, %s and renewal %s as %o', (readState, availability, outcome, causes) => {
    expect(
      toCertificateCauses({
        observation: { availability },
        evidence: { readState, renewal: outcome === undefined ? undefined : { outcome } },
      }),
    ).toEqual(causes);
  });

  it('quotes the observation reason for the read and the renewal reason for the renewal', () => {
    expect(
      toCertificateCauses({
        observation: {
          availability: 'forbidden',
          reason: { code: 'denied', message: 'no access' },
        },
        evidence: {
          readState: 'readable',
          renewal: { outcome: 'failed', reason: { message: 'signer down' } },
        },
      }),
    ).toEqual(['availability forbidden — denied: no access', 'renewal failed — signer down']);
  });
});

describe('toReconciledAttempt', () => {
  const earlier = {
    outcome: 'success',
    attemptedAt: at(-600),
    completedAt: at(-590),
    lastSuccessAt: at(-590),
  };
  const later = { outcome: 'failed', attemptedAt: at(-60), completedAt: at(-50) };

  it('keeps the later attempt with the earlier last success', () => {
    expect([
      toReconciledAttempt(earlier, later, now),
      toReconciledAttempt(later, earlier, now),
    ]).toEqual([
      { ...later, lastSuccessAt: earlier.lastSuccessAt },
      { ...later, lastSuccessAt: earlier.lastSuccessAt },
    ]);
  });

  it('keeps a valid last success over one stamped beyond the allowed skew', () => {
    const skewed = { ...later, outcome: 'success', lastSuccessAt: at(90) };
    expect(toReconciledAttempt(earlier, skewed, now)?.lastSuccessAt).toBe(earlier.lastSuccessAt);
  });

  it('keeps the more recent of two valid last successes', () => {
    const fresh = { ...later, outcome: 'success', lastSuccessAt: at(-50) };
    expect(toReconciledAttempt(earlier, fresh, now)).toEqual(fresh);
  });

  it('is the only attempt when one record carries none', () => {
    expect([
      toReconciledAttempt(undefined, later, now),
      toReconciledAttempt(earlier, null, now),
      toReconciledAttempt(undefined, undefined, now),
    ]).toEqual([later, earlier, undefined]);
  });
});

describe('toDistinctBackups', () => {
  it('shows each backup and each copy once, in the position of the first record', () => {
    const backups = toDistinctBackups(
      [
        { backupID: 'b7', copies: [{ copyID: 'c1', locationID: 'local', kind: 'local' }] },
        { backupID: 'b8', createdAt: 'later', copies: [{ locationID: 'local' }] },
        {
          backupID: 'b7',
          createdAt: '2026-01-05T02:00:00Z',
          sizeBytes: 10,
          copies: [
            { copyID: 'c1', locationID: 'local', save: { outcome: 'success' } },
            { copyID: 'c1', locationID: 's3-east', kind: 'remote' },
          ],
        },
        { createdAt: 'nameless' },
      ],
      now,
    );
    expect(
      backups.map(({ backupID, createdAt, sizeBytes, copies }) => [
        backupID,
        createdAt,
        sizeBytes,
        copies.map(({ copyID, locationID, save }) => [copyID, locationID, save?.outcome]),
      ]),
    ).toEqual([
      [
        'b7',
        '2026-01-05T02:00:00Z',
        10,
        [
          ['c1', 'local', 'success'],
          ['c1', 's3-east', undefined],
        ],
      ],
      ['b8', 'later', undefined, []],
    ]);
  });
});

const success = (secondsAgo: number) => ({
  outcome: 'success',
  attemptedAt: at(-secondsAgo - 10),
  completedAt: at(-secondsAgo),
  lastSuccessAt: at(-secondsAgo),
});

const covered: BackupEvidence = {
  policy: {
    state: 'configured',
    enabled: true,
    requiredLocations: ['local', 's3-east', 'local'],
    maxAgeSeconds: 86400,
    schedule: { state: 'enabled', expression: '0 2 * * *' },
  },
  listing: { complete: true },
  backups: [
    {
      backupID: 'b7',
      copies: [
        { copyID: 'c1', locationID: 'local', kind: 'local', save: success(3600) },
        { copyID: 'c2', locationID: 's3-east', kind: 'remote', upload: success(86400) },
      ],
    },
  ],
};

const withPolicy = (patch: BackupEvidence['policy']): BackupEvidence => ({
  ...covered,
  policy: { ...covered.policy, ...patch },
});

const withRemoteUpload = (upload: object): BackupEvidence => ({
  ...covered,
  backups: [
    {
      backupID: 'b7',
      copies: [
        { copyID: 'c1', locationID: 'local', kind: 'local', save: success(3600) },
        { copyID: 'c2', locationID: 's3-east', kind: 'remote', upload },
      ],
    },
  ],
});

describe('toBackupReading', () => {
  it.each<[string, BackupEvidence, string, string]>([
    [
      'an unconfigured policy',
      withPolicy({ state: 'unconfigured', enabled: false }),
      'unconfigured',
      backupReadings.unconfigured,
    ],
    ['a disabled policy', withPolicy({ state: 'disabled' }), 'disabled', backupReadings.disabled],
    ['a policy switched off', withPolicy({ enabled: false }), 'disabled', backupReadings.disabled],
    [
      'a switched-off policy of an unknown state',
      withPolicy({ state: 'paused', enabled: false }),
      'disabled',
      backupReadings.disabled,
    ],
    ['no policy', { ...covered, policy: undefined }, 'uncertain', backupReadings.unknownPolicy],
    [
      'an unknown state word',
      withPolicy({ state: 'paused' }),
      'uncertain',
      backupReadings.unknownPolicy,
    ],
    [
      'an unstated enabled flag',
      withPolicy({ enabled: undefined }),
      'uncertain',
      backupReadings.unknownPolicy,
    ],
    [
      'no schedule',
      withPolicy({ schedule: undefined }),
      'uncertain',
      backupReadings.unknownSchedule,
    ],
    [
      'an unknown schedule state',
      withPolicy({ schedule: { state: 'paused' } }),
      'uncertain',
      backupReadings.unknownSchedule,
    ],
    [
      'no copy age bound',
      withPolicy({ maxAgeSeconds: undefined }),
      'uncertain',
      backupReadings.unknownAgeBound,
    ],
    [
      'no listing',
      { ...covered, listing: undefined },
      'uncertain',
      backupReadings.incompleteListing,
    ],
    [
      'an incomplete listing',
      { ...covered, listing: { complete: false } },
      'uncertain',
      backupReadings.incompleteListing,
    ],
    [
      'full coverage, the freshest success exactly as old as the bound',
      covered,
      'satisfactory',
      backupReadings.satisfactory,
    ],
    [
      'no required location and no backup',
      { ...withPolicy({ requiredLocations: [] }), backups: [] },
      'satisfactory',
      backupReadings.satisfactory,
    ],
  ])('reads %s', (_case, fragment, word, detail) => {
    expect(toBackupReading(fragment, now)).toEqual({ word, detail });
  });

  it('asks its questions in a fixed order', () => {
    expect(
      toBackupReading({ policy: { state: 'unconfigured' }, listing: { complete: false } }, now)
        .word,
    ).toBe('unconfigured');
  });

  it.each<[string, BackupEvidence, string, string]>([
    [
      'a location with no valid success',
      withRemoteUpload({ outcome: 'running', attemptedAt: at(-60) }),
      'critical',
      'no copy covers required location s3-east',
    ],
    [
      'a location whose only success is stamped beyond the allowed skew',
      withRemoteUpload({ ...success(0), lastSuccessAt: at(31) }),
      'critical',
      'no copy covers required location s3-east',
    ],
    [
      'a stale location',
      withRemoteUpload(success(3 * 86400)),
      'critical',
      'the freshest copy at s3-east is 3d old, past the configured 24h bound',
    ],
    [
      'a failed upload behind fresh coverage',
      withRemoteUpload({ ...success(3600), outcome: 'failed' }),
      'partial',
      'the upload attempt for copy c2 at s3-east of b7 failed',
    ],
  ])('reads %s', (_case, fragment, word, detail) => {
    expect(toBackupReading(fragment, now)).toEqual({ word, detail });
  });

  it('covers a location with an age of zero for a success stamped 10 seconds ahead', () => {
    expect(
      toBackupReading(withRemoteUpload({ ...success(0), lastSuccessAt: at(10) }), now).word,
    ).toBe('satisfactory');
  });

  it('lists every missing location, then every stale one, then every failed attempt', () => {
    const fragment: BackupEvidence = {
      ...withPolicy({ requiredLocations: ['tape', 's3-east', 'local'] }),
      backups: [
        {
          backupID: 'b7',
          copies: [
            {
              copyID: 'c1',
              locationID: 'local',
              kind: 'local',
              save: { ...success(3600), outcome: 'failed' },
            },
            { copyID: 'c2', locationID: 's3-east', kind: 'remote', upload: success(3 * 86400) },
            { copyID: 'c3', locationID: 'tape', kind: 'tape', save: success(60) },
          ],
        },
      ],
    };
    expect(toBackupReading(fragment, now)).toEqual({
      word: 'critical',
      detail:
        'no copy covers required location tape; the freshest copy at s3-east is 3d old, past the configured 24h bound; the save attempt for copy c1 at local of b7 failed',
    });
  });
});

describe('toPosture', () => {
  const pendingRequest = { spec: { signerName: 'kubernetes.io/kubelet-serving' } };
  const reads: PostureReads = {
    facts: undefined,
    lease: undefined,
    leaseFailure: undefined,
    requests: [toApproved(), pendingRequest],
    requestsFailure: undefined,
    namespaces: [toNamespace('prod', { 'pod-security.kubernetes.io/enforce': 'restricted' })],
    nodes: [
      { metadata: { labels: { 'node-role.kubernetes.io/control-plane': '' } } },
      { metadata: { labels: {} } },
    ],
  };
  const facts = {
    id: 'prod',
    name: 'prod',
    observedAt: '2026-01-10T11:00:00Z',
    kubernetesVersion: 'v1.33.0',
    versionDrift: false,
    nodes: [],
    operatorLease: { state: 'active', holder: 'operator-0', renewTime: null },
    kubeletServingCSRPending: 4,
    deployments: [],
    complete: true,
  };

  it('takes the verdict, the pending count and the observation time from the hub facts', () => {
    expect(toPosture({ ...reads, facts }, now)).toEqual({
      operator: { state: 'active', holder: 'operator-0', isRefused: false },
      approved: 1,
      pending: { pending: 4, availability: 'unknown' },
      restricted: { names: ['prod'], restrictedCount: 1, namespaceCount: 1 },
      controlPlaneCount: 1,
      observedAt: '2026-01-10T11:00:00Z',
    });
  });

  it('leaves the approved count unknown beside the hub pending count when the list is unreadable', () => {
    const posture = toPosture(
      {
        ...reads,
        facts: { ...facts, kubeletServingCSRAvailability: 'available' },
        requests: undefined,
        requestsFailure: { status: 403, payload: {} },
      },
      now,
    );
    expect([posture.approved, posture.pending]).toEqual([
      undefined,
      { pending: 4, availability: 'available' },
    ]);
  });

  it('derives the verdict and the counts from the cluster without hub facts', () => {
    const lease = { spec: { holderIdentity: 'operator-0', renewTime: at(-20) } };
    expect([
      toPosture({ ...reads, lease }, now).operator,
      toPosture({ ...reads, leaseFailure: { status: 403, payload: {} } }, now).operator,
      toPosture({ ...reads, leaseFailure: { status: 404, payload: {} } }, now).operator.state,
      toPosture(reads, now).pending,
      toPosture(
        { ...reads, requests: undefined, requestsFailure: { status: 403, payload: {} } },
        now,
      ).pending,
      toPosture(
        { ...reads, requests: undefined, requestsFailure: { status: 500, payload: {} } },
        now,
      ).pending,
    ]).toEqual([
      { state: 'active', holder: 'operator-0', isRefused: false },
      { state: 'unknown', holder: '—', isRefused: true },
      'missing',
      { pending: 1, availability: 'available' },
      { pending: undefined, availability: 'forbidden' },
      { pending: undefined, availability: 'unknown' },
    ]);
  });

  it('leaves the PodSecurity numbers unknown when the namespaces could not be read', () => {
    expect(toPosture({ ...reads, namespaces: undefined }, now).restricted).toEqual({
      names: undefined,
      restrictedCount: undefined,
      namespaceCount: undefined,
    });
  });
});

describe('toPostureLoad', () => {
  it('tells loading from unreachable', () => {
    expect([
      toPostureLoad({ data: undefined, failureCount: 0, failureReason: null }),
      toPostureLoad({ data: undefined, failureCount: 2, failureReason: {} }),
      toPostureLoad({ data: { items: [] }, failureCount: 1, failureReason: {} }),
    ]).toEqual(['loading', 'unreachable', 'ready']);
  });
});
