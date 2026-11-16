import type { Evidence, EvidencePage, ReportDetail } from '~/api/fleet/fleetSchemas';
import type { Namespace } from '~/api/k8s/k8sSchemas';
import { hasLabel, isRecord } from '~/shared/utils/predicates';

import {
  approvedCondition,
  backupSignal,
  certificateSignal,
  cleanAvailability,
  cleanRenewalOutcome,
  clockSkewMs,
  decidingConditions,
  kubeletServingSigner,
  podSecurityEnforceLabel,
  restrictedLevel,
  supervisorType,
} from '../constants';
import type { PendingRequests, SigningRequest } from '../types';
import type {
  FindingFilters,
  FindingView,
  ObservationView,
  OpenedEvidence,
  Stated,
} from '../types/engineState';
import type { BackupAttempt } from '../types/evidence';
import { fromTimestamp } from './parsers';

const hasCondition = ({ status }: SigningRequest, types: string[]): boolean =>
  (status?.conditions ?? []).some((condition) => types.includes(condition.type));

export const isKubeletServingRequest = ({ spec }: SigningRequest): boolean =>
  spec.signerName === kubeletServingSigner;

export const isApprovedRequest = (request: SigningRequest): boolean =>
  hasCondition(request, [approvedCondition]);

export const isPendingRequest = (request: SigningRequest): boolean =>
  !hasCondition(request, decidingConditions) && !request.status?.certificate;

export const isRestrictedNamespace = (namespace: Pick<Namespace, 'metadata'>): boolean =>
  hasLabel(namespace, podSecurityEnforceLabel, restrictedLevel);

export const isPendingKnown = ({ pending, availability }: PendingRequests): boolean =>
  availability === cleanAvailability && pending !== undefined;

export const isReportDetail = (page: unknown): page is ReportDetail =>
  isRecord(page) && Array.isArray(page.findings);

export const isEvidencePage = (page: unknown): page is EvidencePage =>
  isRecord(page) && Array.isArray(page.items);

export const isSameFilters = (first: FindingFilters, second: FindingFilters): boolean =>
  first.signal === second.signal &&
  first.nodeUID === second.nodeUID &&
  first.minimumSeverity === second.minimumSeverity;

export const hasEvidence = ({ evidenceRef }: FindingView): boolean =>
  evidenceRef !== undefined && evidenceRef !== null;

export const isEvidenceOpen = (
  { evidenceRef }: FindingView,
  opened: Pick<OpenedEvidence, 'evidenceRef'> | undefined,
): boolean => opened !== undefined && evidenceRef === opened.evidenceRef;

export const isTypedSignal = (signal: string | undefined): boolean =>
  signal === certificateSignal || signal === backupSignal;

export const isShownRaw = ({ type }: Evidence, signal: string | undefined): boolean =>
  type !== supervisorType && !(isTypedSignal(signal) && type === signal);

export const isWithinSkew = (time: number, now: number): boolean => time - now <= clockSkewMs;

export const isAttemptFailed = (attempt: Stated<BackupAttempt>): boolean =>
  attempt !== undefined &&
  attempt !== null &&
  fromTimestamp(attempt.completedAt) !== undefined &&
  (attempt.outcome !== cleanRenewalOutcome || fromTimestamp(attempt.lastSuccessAt) === undefined);

export const isLaterAttempt = (first: BackupAttempt, second: BackupAttempt): boolean => {
  const firstAttempted = fromTimestamp(first.attemptedAt);
  const secondAttempted = fromTimestamp(second.attemptedAt);
  const firstCompleted = fromTimestamp(first.completedAt);
  const secondCompleted = fromTimestamp(second.completedAt);
  return firstAttempted !== secondAttempted
    ? (firstAttempted ?? 0) > (secondAttempted ?? 0)
    : (firstCompleted === undefined) !== (secondCompleted === undefined)
      ? firstCompleted !== undefined
      : firstCompleted !== secondCompleted
        ? (firstCompleted ?? 0) > (secondCompleted ?? 0)
        : isAttemptFailed(first) !== isAttemptFailed(second)
          ? isAttemptFailed(first)
          : (first.outcome ?? '') < (second.outcome ?? '');
};

export const isLaterSuccess = (
  first: number | undefined,
  second: number | undefined,
  now: number,
): boolean =>
  first === undefined || second === undefined
    ? first !== undefined
    : isWithinSkew(first, now) !== isWithinSkew(second, now)
      ? isWithinSkew(first, now)
      : first > second;

export const isReadStanding = (observation: ObservationView | undefined): boolean =>
  observation?.availability === undefined ||
  observation.availability === null ||
  observation.availability === cleanAvailability;

export const isSameEvidence = (
  first: Pick<OpenedEvidence, 'evidenceRef' | 'reportGeneration'> | undefined,
  second: Pick<OpenedEvidence, 'evidenceRef' | 'reportGeneration'> | undefined,
): boolean =>
  first !== undefined &&
  second !== undefined &&
  first.evidenceRef === second.evidenceRef &&
  first.reportGeneration === second.reportGeneration;
