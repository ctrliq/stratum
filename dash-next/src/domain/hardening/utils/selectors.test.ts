import { describe, expect, it } from 'vitest';

import { dashboardConfig } from '~/domain/auth/testing/fixtures';
import { unresolvedConfig } from '~/shared/context/ConfigContext';

import type { SigningRequest } from '../types';
import { isEvidencePage, isReportDetail } from './predicates';
import {
  approvedCountOf,
  certificateObservationsOf,
  cisProfileOf,
  copySuccessTimeOf,
  currentGenerationOf,
  detailGenerationOf,
  itemsOf,
  itemsOfType,
  nodesOf,
  observationFor,
  observationKeyOf,
  observedAtOf,
  pendingCountOf,
  shapeFaultOf,
  signalsOf,
} from './selectors';

const toRequest = (signerName: string, conditionTypes: string[] = []): SigningRequest => ({
  spec: { signerName },
  status: { conditions: conditionTypes.map((type) => ({ type, status: 'True' })) },
});

const requests = [
  toRequest('kubernetes.io/kubelet-serving', ['Approved']),
  toRequest('kubernetes.io/kubelet-serving', ['Approved']),
  toRequest('kubernetes.io/kubelet-serving', ['Denied']),
  toRequest('kubernetes.io/kubelet-serving'),
  toRequest('kubernetes.io/kube-apiserver-client'),
  toRequest('kubernetes.io/kube-apiserver-client', ['Approved']),
];

describe('approvedCountOf', () => {
  it('counts the approved kubelet-serving requests', () => {
    expect(approvedCountOf(requests)).toBe(2);
  });
});

describe('pendingCountOf', () => {
  it('counts the kubelet-serving requests nothing decided, ignoring other signers', () => {
    expect(pendingCountOf(requests)).toBe(1);
  });
});

describe('observedAtOf', () => {
  it('is the hub’s observation time', () => {
    expect(observedAtOf({ observedAt: '2026-11-13T11:58:00Z' })).toBe('2026-11-13T11:58:00Z');
  });

  it('is absent before the hub’s first observation', () => {
    expect(observedAtOf({ observedAt: null })).toBeUndefined();
  });

  it('is absent when no hub answered', () => {
    expect(observedAtOf(undefined)).toBeUndefined();
  });
});

const subject = { scope: 'node', nodeUID: 'uid-1', component: 'kubelet', key: 'serving' };
const findings = [
  { signal: 'certificate', subject },
  { signal: 'version', subject: { scope: 'node', nodeUID: 'uid-2' } },
  { signal: 'certificate', subject: { scope: 'cluster' } },
];

describe('cisProfileOf', () => {
  it('is the control list the hub serves, empty when it serves none', () => {
    const control = { id: '1.1', desc: 'd', group: 'API Server', source: 'a.yaml' };
    expect([
      cisProfileOf({ ...unresolvedConfig, cisProfile: [control] }),
      cisProfileOf(dashboardConfig),
    ]).toEqual([[control], []]);
  });
});

describe('observationFor', () => {
  it('matches a finding to the observation with the same signal and subject', () => {
    const matching = { signal: 'certificate', subject, availability: 'available' };
    const other = { signal: 'version', subject };
    expect([
      observationFor([other, matching], findings[0]),
      observationFor([other], findings[0]),
      observationKeyOf(matching) === observationKeyOf(findings[0]),
    ]).toEqual([matching, undefined, true]);
  });
});

describe('certificateObservationsOf', () => {
  it('keeps certificate observations in the order served', () => {
    expect(
      certificateObservationsOf([{ signal: 'version' }, { signal: 'certificate', id: 'a' }]),
    ).toEqual([{ signal: 'certificate', id: 'a' }]);
  });
});

describe('signalsOf and nodesOf', () => {
  it('offer what the walked findings contain, once each', () => {
    expect([signalsOf(findings, ''), nodesOf(findings, '')]).toEqual([
      ['certificate', 'version'],
      ['uid-1', 'uid-2'],
    ]);
  });

  it('keep a selected value the narrowed findings no longer list', () => {
    expect([signalsOf([], 'version'), nodesOf([], 'uid-9')]).toEqual([['version'], ['uid-9']]);
  });
});

describe('detailGenerationOf and currentGenerationOf', () => {
  it('name the report with retained detail and the generation now current', () => {
    const summary = { reportGeneration: 'g-8', detail: { reportGeneration: 'g-8' } };
    expect([
      detailGenerationOf(summary),
      detailGenerationOf({ reportGeneration: 'g-8' }),
      detailGenerationOf(undefined),
      currentGenerationOf(summary, undefined),
      currentGenerationOf({ reportGeneration: 'g-7' }, { currentReportGeneration: 'g-8' }),
      currentGenerationOf(undefined, undefined),
    ]).toEqual(['g-8', undefined, undefined, 'g-8', 'g-8', undefined]);
  });
});

describe('itemsOf and itemsOfType', () => {
  it('keep the stated items, and those of one type', () => {
    const items = itemsOf({ items: [{ type: 'backup' }, null, { type: 'version' }, {}] });
    expect([items, itemsOfType(items, 'backup'), itemsOf({})]).toEqual([
      [{ type: 'backup' }, { type: 'version' }, {}],
      [{ type: 'backup' }],
      [],
    ]);
  });
});

describe('copySuccessTimeOf', () => {
  const save = { lastSuccessAt: '2026-01-05T02:00:00Z' };
  const upload = { lastSuccessAt: '2026-01-05T03:00:00Z' };
  it.each([
    ['remote', Date.UTC(2026, 0, 5, 3)],
    ['local', Date.UTC(2026, 0, 5, 2)],
    ['tape', undefined],
  ])('a %s copy succeeded at %s', (kind, expected) => {
    expect(copySuccessTimeOf({ kind, save, upload })).toBe(expected);
  });
});

describe('shapeFaultOf', () => {
  const findingsFault = 'the findings route did not answer a ReportDetail page';
  const evidenceFault = 'the evidence route did not answer an EvidencePage';

  it.each([[undefined], [null], ['text'], [[1, 2]]])(
    'reports a success whose body is %s as not a JSON object',
    (answer) => {
      expect(shapeFaultOf(answer, isReportDetail, findingsFault)).toBe(
        'the engine-state response was not a JSON object',
      );
    },
  );

  it('reports a JSON object that lacks a list of findings with the findings route message', () => {
    expect(shapeFaultOf({ state: 'available' }, isReportDetail, findingsFault)).toBe(findingsFault);
  });

  it('reports a JSON object that lacks a list of items with the evidence route message', () => {
    expect(shapeFaultOf({ complete: true }, isEvidencePage, evidenceFault)).toBe(evidenceFault);
  });

  it('finds no fault in an answer of the right shape', () => {
    expect([
      shapeFaultOf({ findings: [] }, isReportDetail, findingsFault),
      shapeFaultOf({ items: [] }, isEvidencePage, evidenceFault),
    ]).toEqual([undefined, undefined]);
  });
});
