import type {
  DetailRef,
  Evidence,
  EvidencePage,
  Finding,
  Observation,
  Reason,
  ReportDetail,
  StateSummary,
} from '~/api/fleet/fleetSchemas';

export type Stated<TValue> = TValue | null | undefined;

export type Loose<TValue> = TValue extends (infer TItem)[]
  ? Loose<TItem>[]
  : TValue extends object
    ? { [TKey in keyof TValue]?: Loose<TValue[TKey]> | null }
    : TValue;

export type SummaryView = Omit<Partial<StateSummary>, 'detail'> & {
  detail?: Partial<DetailRef> | null;
};
export type FindingView = Loose<Finding>;
export type ObservationView = Omit<Loose<Observation>, 'evidence'> & {
  evidence?: Stated<Evidence>;
};
export type EvidencePageView = Omit<Loose<EvidencePage>, 'items'> & {
  items?: Stated<Stated<Evidence>[]>;
};
export type ReportDetailView = Omit<Loose<ReportDetail>, 'observations'> & {
  observations?: Stated<ObservationView[]>;
};
export type ReasonView = Loose<Reason>;
export type SubjectView = NonNullable<FindingView['subject']>;

export type FindingFilters = {
  signal: string;
  nodeUID: string;
  minimumSeverity: string;
};

export type Refusal = {
  status: number | 'unknown';
  message: string;
  isRetryable: boolean;
  code?: string;
  requestId?: string;
  currentReportGeneration?: string;
  retainedFrom?: string;
  retainedTo?: string;
};

export type FindingsWalk = {
  reportGeneration: string;
  filters: FindingFilters;
  findings: FindingView[];
  observations: ObservationView[];
  totalFindings: number;
  isComplete: boolean;
  isCut: boolean;
  state: string | undefined;
  issuedCursors: string[];
  nextCursor: string | undefined;
};

export type FindingsRequest = {
  clusterId: string;
  reportGeneration: string;
  filters: FindingFilters;
};

export type Retained<TValue> = {
  scope: string | undefined;
  value: TValue | undefined;
};

export type OpenedEvidence = {
  evidenceRef: string;
  reportGeneration: string;
  signal: string | undefined;
  severity: string | undefined;
  observation: ObservationView | undefined;
  certificateObservations: ObservationView[];
};

export type RoleCoverage = Record<
  'expected' | 'evaluated' | 'unknown' | 'notApplicable',
  number
> & {
  role: string;
};

export type LabelledValue = {
  label: string;
  value: string;
};
