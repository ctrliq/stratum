import type {
  ClusterIdentity,
  Coverage,
  Evidence,
  Source,
  StateSummary,
} from '~/api/fleet/fleetSchemas';
import type { FindingsWalk } from '~/domain/hardening/types/engineState';

type Extended = Record<string, unknown>;

export type VersionReason = Extended & {
  code: string | undefined;
  message: string | undefined;
};

export type Artifact = Extended & {
  releaseID: string | undefined;
  digest: string | undefined;
};

export type IdentityValue = Extended & {
  raw: string | undefined;
  availability: string | undefined;
  digest: string | undefined;
  reason: VersionReason | undefined;
};

export type Identity = IdentityValue & {
  instances?: IdentityValue[];
};

export type RolloutIntent = Extended & {
  operationID: string | undefined;
  targetUID: string | undefined;
  phase: string | undefined;
  observedAt: string | undefined;
  deadline: string | undefined;
  action: string | undefined;
  targetRelease: Artifact | undefined;
};

export type SampleSource = Partial<Record<keyof Source, string>>;

export type Cause = {
  code: string | undefined;
  severity: string | undefined;
  message: string | undefined;
  sampledAt: string | undefined;
  isPolicy: boolean;
};

export type VersionRow = {
  key: string;
  nodeUID: string | undefined;
  component: string | undefined;
  verdict: string | undefined;
  rule: string | undefined;
  comparisonReason: VersionReason | undefined;
  observationReason: VersionReason | undefined;
  baselineReason: VersionReason | undefined;
  expected: Identity | undefined;
  installed: Identity | undefined;
  serving: Identity | undefined;
  servingLabel: 'Installed' | 'Running' | undefined;
  memberRelease: Artifact | undefined;
  intent: RolloutIntent | undefined;
  intentLabel: string | undefined;
  rolloutLabel: string | undefined;
  severity: string;
  causes: Cause[];
  sampledAt: string | undefined;
  source: SampleSource | undefined;
  evidenceRef: string | undefined;
};

export type VersionLoad = {
  summary: StateSummary | undefined;
  detail:
    | Pick<
        FindingsWalk,
        'reportGeneration' | 'isComplete' | 'isCut' | 'state' | 'findings' | 'observations'
      >
    | undefined;
  evidenceByRef: Record<string, Evidence[]>;
};

export type VersionView =
  | { isReachable: false; rows: VersionRow[] }
  | {
      isReachable: true;
      cluster: ClusterIdentity;
      reportGeneration: string;
      severity: string;
      freshness: string;
      observedAt: string | undefined;
      coverage: Coverage | undefined;
      isTruncated: boolean;
      isPartial: boolean;
      rows: VersionRow[];
    };

export type ReachableVersionView = Extract<VersionView, { isReachable: true }>;
