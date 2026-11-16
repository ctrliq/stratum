import type { Operation, OperationTargetResult } from '~/api/fleet/fleetSchemas';

export const toTarget = (
  overrides: Partial<OperationTargetResult> = {},
): OperationTargetResult => ({
  childOperationID: 'child-1',
  target: {
    cluster: { name: 'prod', uid: 'uid-prod', memberUID: 'member-prod' },
    nodeUID: 'node-1',
    bindingUID: 'binding-1',
    sourceRevision: 'rev-1',
  },
  state: 'running',
  outcome: 'unknown',
  phase: 'pending',
  committed: false,
  resultRefs: [],
  causes: [],
  targetSet: { selectionID: 'sel-1', selectionDigest: 'digest', memberCount: 1 },
  ...overrides,
});

export const toOperation = (overrides: Partial<Operation> = {}): Operation => ({
  schemaVersion: 'v1',
  kind: 'Operation',
  operationID: 'op-1',
  action: 'certificates.check',
  acceptedAt: '2026-11-13T12:00:00Z',
  deadline: '2026-11-13T13:00:00Z',
  state: 'running',
  outcome: 'unknown',
  targets: [toTarget()],
  nextCursor: null,
  targetCount: 5,
  terminalCount: 2,
  committedCount: 1,
  countsByState: {},
  countsByOutcome: { success: 1, failed: 1 },
  ...overrides,
});
