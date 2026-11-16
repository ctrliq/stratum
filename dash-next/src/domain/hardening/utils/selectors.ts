import type { CisControl, ClusterFacts, Evidence } from '~/api/fleet/fleetSchemas';
import type { ServerConfig } from '~/shared/context/ConfigContext';
import { isRecord } from '~/shared/utils/predicates';

import { certificateSignal, notAnObjectMessage } from '../constants';
import type { SigningRequest } from '../types';
import type {
  EvidencePageView,
  FindingView,
  ObservationView,
  Refusal,
  Stated,
  SubjectView,
  SummaryView,
} from '../types/engineState';
import type { BackupCopy } from '../types/evidence';
import { kubeletServingRequestsIn } from './filters';
import { fromTimestamp } from './parsers';
import { isApprovedRequest, isPendingRequest } from './predicates';

export const approvedCountOf = (requests: SigningRequest[]): number =>
  kubeletServingRequestsIn(requests).filter(isApprovedRequest).length;

export const pendingCountOf = (requests: SigningRequest[]): number =>
  kubeletServingRequestsIn(requests).filter(isPendingRequest).length;

export const observedAtOf = (
  facts: Pick<ClusterFacts, 'observedAt'> | undefined,
): string | undefined => facts?.observedAt ?? undefined;

export const cisProfileOf = (config: ServerConfig): CisControl[] =>
  'cisProfile' in config && Array.isArray(config.cisProfile) ? config.cisProfile : [];

export const observationKeyOf = ({
  signal,
  subject,
}: {
  signal?: Stated<string>;
  subject?: Stated<SubjectView>;
}): string =>
  JSON.stringify([signal, subject?.scope, subject?.nodeUID, subject?.component, subject?.key]);

export const observationFor = (
  observations: ObservationView[],
  finding: FindingView,
): ObservationView | undefined =>
  observations.find((observation) => observationKeyOf(observation) === observationKeyOf(finding));

export const certificateObservationsOf = (observations: ObservationView[]): ObservationView[] =>
  observations.filter(({ signal }) => signal === certificateSignal);

const distinctOf = (values: Stated<string>[], selected: string): string[] => [
  ...new Set([...values, selected].filter((value): value is string => Boolean(value))),
];

export const signalsOf = (findings: FindingView[], selected: string): string[] =>
  distinctOf(
    findings.map(({ signal }) => signal),
    selected,
  );

export const nodesOf = (findings: FindingView[], selected: string): string[] =>
  distinctOf(
    findings.map(({ subject }) => subject?.nodeUID),
    selected,
  );

export const detailGenerationOf = (summary: SummaryView | undefined): string | undefined =>
  summary?.detail?.reportGeneration ?? undefined;

export const currentGenerationOf = (
  summary: SummaryView | undefined,
  refusal: Pick<Refusal, 'currentReportGeneration'> | undefined,
): string | undefined => refusal?.currentReportGeneration ?? summary?.reportGeneration ?? undefined;

export const itemsOf = ({ items }: EvidencePageView): Evidence[] =>
  (items ?? []).filter((item) => item !== undefined && item !== null);

export const itemsOfType = (items: Evidence[], type: string): Evidence[] =>
  items.filter((item) => item.type === type);

export const copySuccessTimeOf = ({ kind, save, upload }: BackupCopy): number | undefined =>
  kind === 'local'
    ? fromTimestamp(save?.lastSuccessAt)
    : kind === 'remote'
      ? fromTimestamp(upload?.lastSuccessAt)
      : undefined;

export const shapeFaultOf = (
  answer: unknown,
  isShaped: (answer: unknown) => boolean,
  routeFault: string,
): string | undefined =>
  !isRecord(answer) ? notAnObjectMessage : isShaped(answer) ? undefined : routeFault;
