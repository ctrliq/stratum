import { findingsPageLimit } from '../constants';
import type {
  FindingsRequest,
  FindingsWalk,
  FindingView,
  ObservationView,
  OpenedEvidence,
  ReportDetailView,
  Retained,
} from '../types/engineState';
import { hasEvidence, isSameEvidence } from './predicates';
import { certificateObservationsOf, observationFor, observationKeyOf } from './selectors';

export const toEmptyWalk = ({
  reportGeneration,
  filters,
}: Pick<FindingsRequest, 'reportGeneration' | 'filters'>): FindingsWalk => ({
  reportGeneration,
  filters,
  findings: [],
  observations: [],
  totalFindings: 0,
  isComplete: false,
  isCut: false,
  state: undefined,
  issuedCursors: [],
  nextCursor: undefined,
});

const withObservations = (
  kept: ObservationView[],
  served: ObservationView[],
): ObservationView[] => [
  ...new Map(
    [...kept, ...served].map((observation) => [observationKeyOf(observation), observation]),
  ).values(),
];

export const withPage = (walk: FindingsWalk, page: ReportDetailView): FindingsWalk => {
  const findings = [...walk.findings, ...(page.findings ?? [])];
  const cursor = page.nextCursor || undefined;
  const isCut =
    cursor !== undefined &&
    (walk.issuedCursors.includes(cursor) || walk.issuedCursors.length + 1 >= findingsPageLimit);
  return {
    ...walk,
    findings,
    observations: withObservations(walk.observations, page.observations ?? []),
    totalFindings: typeof page.totalFindings === 'number' ? page.totalFindings : findings.length,
    isComplete: page.complete === true && !isCut,
    isCut,
    state: page.state || undefined,
    issuedCursors: cursor === undefined ? walk.issuedCursors : [...walk.issuedCursors, cursor],
    nextCursor: isCut ? undefined : cursor,
  };
};

export const withOpenedFinding = (
  opened: OpenedEvidence | undefined,
  finding: FindingView,
  walk: FindingsWalk,
): OpenedEvidence | undefined => {
  const severity = finding.severity ?? undefined;
  const snapshot: OpenedEvidence = {
    evidenceRef: finding.evidenceRef ?? '',
    reportGeneration: walk.reportGeneration,
    signal: finding.signal || undefined,
    severity,
    observation: observationFor(walk.observations, finding),
    certificateObservations: certificateObservationsOf(walk.observations),
  };
  return !hasEvidence(finding)
    ? opened
    : opened !== undefined && isSameEvidence(opened, snapshot)
      ? { ...opened, severity }
      : snapshot;
};

export const withRetained = <TValue>(
  held: Retained<TValue>,
  scope: string | undefined,
  value: TValue | undefined,
): Retained<TValue> =>
  held.scope === scope && (value === undefined || value === held.value) ? held : { scope, value };
