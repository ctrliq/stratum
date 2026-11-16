import type { Finding, Observation, StateSummary } from '~/api/fleet/fleetSchemas';

import type { VersionLoad } from '../types/versions';
import { toVersionView } from '../utils/transforms';

export const cluster = { name: 'edge', uid: 'c-1', memberUID: 'm-1' };
const coverage = {
  expectationRevision: 'e-1',
  expected: 6,
  evaluated: 5,
  unknown: 1,
  notApplicable: 0,
  complete: false,
};

export const summary: StateSummary = {
  schemaVersion: 'v1',
  kind: 'StateSummary',
  cluster,
  reportGeneration: 'g-1',
  observedAt: '2026-01-05T10:00:00Z',
  evaluatedAt: '2026-01-05T10:00:05Z',
  severity: 'warning',
  freshness: 'fresh',
  coverage,
  findingCounts: { total: 0, required: 0, optional: 0, bySeverity: {} },
  detail: {
    reportGeneration: 'g-1',
    manifestID: 'm-1',
    sha256: '',
    totalItems: 0,
    totalBytes: 0,
    complete: true,
    expiresAt: '2026-01-12T10:00:00Z',
  },
};

export const subject = { scope: 'node', nodeUID: 'n-1', component: 'kubelet', key: 'version' };

export const finding = (overrides: Partial<Finding>): Finding => ({
  id: 'f-1',
  signal: 'version',
  subject,
  source: null,
  severity: 'warning',
  required: true,
  active: true,
  applicable: true,
  code: 'outcome-drift',
  message: 'installed differs from expected',
  sampledAt: '2026-01-05T09:59:00Z',
  evidenceRef: null,
  ...overrides,
});

export const policy = {
  kind: 'hub-policy',
  policyRevision: 'p',
  expectationRevision: 'e',
  evaluatedAt: '',
};

export const observation = (evidence: Observation['evidence']): Observation => ({
  cluster,
  id: 'o-1',
  subject,
  source: {
    bindingUID: 'b-1',
    memberUID: 'm-1',
    nodeUID: 'n-1',
    producer: 'agent',
    build: 'b7',
    processInstance: 'p-1',
    processEpoch: '1',
    sequence: '1',
    startedAt: '2026-01-05T09:00:00Z',
  },
  signal: 'version',
  times: {
    sampledAt: '2026-01-05T09:59:00Z',
    attemptedAt: '2026-01-05T09:59:00Z',
    completedAt: '2026-01-05T09:59:01Z',
    lastSuccessAt: '2026-01-05T09:59:00Z',
  },
  availability: 'available',
  applicability: 'applicable',
  nativeOutcome: '',
  coverage,
  evidence,
  reason: { code: 'sampled', message: 'ok' },
  evidenceRef: null,
});

export const load = (
  findings: Finding[],
  observations: Observation[] = [],
  detail: Partial<NonNullable<VersionLoad['detail']>> = {},
): VersionLoad => ({
  summary,
  detail: {
    reportGeneration: 'g-1',
    isComplete: true,
    isCut: false,
    state: 'available',
    findings,
    observations,
    ...detail,
  },
  evidenceByRef: {},
});

export const rowsOf = (input: VersionLoad) => toVersionView(input).rows;
