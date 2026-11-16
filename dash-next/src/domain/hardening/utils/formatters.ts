import { toSeverityTone } from '~/domain/clusters/utils/formatters';
import type { Tone } from '~/shared/types/tone';
import { absentText, toAgeAgo, toDisplayText, toLocalTime } from '~/shared/utils/formatters';

import {
  controlTagLabels,
  criticalExpiryWindowMs,
  drilldownPathPrefix,
  engineStateSegment,
  observedCaption,
  unknownCount,
  unknownSeverity,
  unlabelledWarning,
  warningExpiryWindowMs,
} from '../constants';
import type { ControlGroup, OperatorVerdict } from '../types';
import type {
  EvidencePageView,
  FindingFilters,
  FindingsWalk,
  FindingView,
  LabelledValue,
  ObservationView,
  ReasonView,
  Refusal,
  RoleCoverage,
  Stated,
} from '../types/engineState';
import type {
  BackupAttempt,
  BackupEvidence,
  BackupListing,
  BackupPolicy,
  CertificateEvidence,
  DistinctBackup,
  DistinctCopy,
  SupervisedChild,
  SupervisorEvidence,
  SupervisorObservation,
} from '../types/evidence';
import { fromTimestamp } from './parsers';
import { isWithinSkew } from './predicates';
import { copySuccessTimeOf } from './selectors';

const separator = ' · ';
const secondsPerMinute = 60;
const minutesPerHour = 60;
const hoursPerDay = 24;
const lastHoursOfCoarseDuration = 48;
const agoSuffix = / ago$/;
export const toElapsed = (time: Stated<string>, now: number): string | undefined =>
  toAgeAgo(time, now)?.replace(agoSuffix, '');

export const toCoarseDuration = (seconds: number): string => {
  const whole = Math.max(0, Math.floor(seconds));
  const minutes = Math.floor(whole / secondsPerMinute);
  const hours = Math.floor(minutes / minutesPerHour);
  return whole < secondsPerMinute
    ? `${whole}s`
    : minutes < minutesPerHour
      ? `${minutes}m`
      : hours < lastHoursOfCoarseDuration
        ? `${hours}h`
        : `${Math.floor(hours / hoursPerDay)}d`;
};

export const toRemaining = (milliseconds: number): string => {
  const seconds = Math.floor(milliseconds / 1000);
  const minutes = Math.floor(seconds / secondsPerMinute);
  const hours = Math.floor(minutes / minutesPerHour);
  return `${Math.floor(hours / hoursPerDay)}d ${hours % hoursPerDay}h ${minutes % minutesPerHour}m ${seconds % secondsPerMinute}s`;
};

export const toReasonText = (reason: Stated<ReasonView>): string =>
  `${reason?.code ?? ''}: ${reason?.message ?? ''}${reason?.retryable ? `${separator}retryable` : ''}`;

export const toOperatorLabel = (state: string): string =>
  state === 'active'
    ? 'Active'
    : state === 'inactive' || state === 'missing'
      ? 'Inactive'
      : 'Unknown';

export const toOperatorDetail = ({ state, isRefused }: OperatorVerdict): string =>
  state === 'active'
    ? 'operator lease held'
    : state === 'inactive'
      ? 'operator lease stale or held by nobody'
      : state === 'missing'
        ? 'no operator lease found'
        : isRefused
          ? "unknown: this cluster's credential cannot read the operator lease"
          : state === 'unknown'
            ? 'no verdict yet — the cluster has not reported the operator lease, or the read failed'
            : `no verdict from state "${state}" — a newer hub's value`;

export const toObservedCaption = (observedAt: string | undefined): string => {
  const at = fromTimestamp(observedAt);
  return at === undefined
    ? observedCaption
    : `${observedCaption}${separator}observed ${new Date(at).toLocaleTimeString()}`;
};

export const toApprovedText = (approved: number | undefined): string =>
  approved === undefined ? unknownCount : String(approved);

export const toProfileTitle = (controlCount: number): string =>
  `CIS control profile (${controlCount})`;

export const toGroupHeading = ({ group, sources }: ControlGroup): string =>
  `${group}${separator}${sources.join(', ')}`;

export const toControlBadge = (tag: string | undefined): string | undefined =>
  controlTagLabels[tag ?? ''];

export const toEngineStatePath = (clusterId: string, listSearch = ''): string =>
  `${drilldownPathPrefix}/${encodeURIComponent(clusterId)}/${engineStateSegment}${listSearch}`;

export const toClusterListPath = (listSearch: string): string =>
  `${drilldownPathPrefix}${listSearch}`;

export const toDrilldownTitle = (clusterName: string | undefined, clusterId: string): string =>
  `Engine state — ${clusterName || clusterId}`;

export const toSummaryRefusal = ({ message }: Refusal, hasSummary: boolean): string =>
  hasSummary ? `the latest read was refused — ${message}` : `engine state unavailable — ${message}`;

export const toFindingsHeader = ({ totalFindings, state }: FindingsWalk): string =>
  `${totalFindings} findings${separator}${state ?? absentText}`;

export const toPartialFindingsNote = ({ findings, totalFindings }: FindingsWalk): string =>
  `partial — ${findings.length} of ${totalFindings} findings served`;

export const toPreviousReportNote = (
  walkedGeneration: string,
  summaryGeneration: Stated<string>,
): string =>
  `these findings are from the previous report — ${walkedGeneration}; the summary describes ${summaryGeneration || 'a report without retained detail'}.`;

export const toFindingsRefusal = ({
  status,
  message,
  isRetryable,
  currentReportGeneration,
  retainedFrom,
  retainedTo,
}: Refusal): string =>
  status === 409
    ? `the report changed while reading${currentReportGeneration ? ` — the generation moved to ${currentReportGeneration}` : ''}; these findings are from the previous report.`
    : status === 410
      ? `report detail expired${retainedFrom || retainedTo ? ` — retained ${retainedFrom || absentText} to ${retainedTo || absentText}` : ''}; the summary above stays current.`
      : `findings unavailable — ${message}${isRetryable ? `${separator}retryable` : ''}`;

export const toRetainedFiltersNote = (
  { signal, nodeUID, minimumSeverity }: FindingFilters,
  isLoading: boolean,
): string =>
  `retained findings for ${[
    signal || 'all signals',
    nodeUID || 'all nodes',
    minimumSeverity ? `minimum severity ${minimumSeverity}` : 'all severities',
  ].join(separator)}; the selected filters ${isLoading ? 'are loading' : 'could not be loaded'}.`;

export const toSeverityText = ({ severity }: FindingView): string => severity || unknownSeverity;

export const toFindingDetail = ({ signal, subject, sampledAt }: FindingView): string =>
  [
    signal || 'unknown signal',
    subject?.scope || 'unknown scope',
    ...(subject?.nodeUID ? [`node ${subject.nodeUID}`] : []),
    ...(subject?.component ? [subject.component] : []),
    ...(subject?.key ? [subject.key] : []),
    `sampled ${toLocalTime(sampledAt)}`,
  ].join(separator);

export const toProcessLine = (source: NonNullable<ObservationView['source']>): string =>
  `Process ${source.processInstance ?? ''}${separator}build ${source.build ?? ''}${separator}started ${toLocalTime(source.startedAt)}`;

export const toCollectionTimes = ({ times }: ObservationView): LabelledValue[] => [
  { label: 'Sampled', value: toLocalTime(times?.sampledAt) },
  { label: 'Attempted', value: toLocalTime(times?.attemptedAt) },
  { label: 'Completed', value: toLocalTime(times?.completedAt) },
  { label: 'Last success', value: toLocalTime(times?.lastSuccessAt) },
];

export const toEvidenceTitle = (evidenceRef: string): string => `Evidence — ${evidenceRef}`;

export const toEvidenceRefusal = ({ message, isRetryable }: Refusal): string =>
  `evidence unavailable — ${message}${isRetryable ? `${separator}retryable` : ''}`;

export const toPartialEvidenceNote = ({ items, totalItems }: EvidencePageView): string =>
  `partial — ${items?.length ?? 0} of ${toDisplayText(totalItems)} items served`;

export const toPreviousEvidenceNote = (reportGeneration: string): string =>
  `this evidence is from the previous report — ${reportGeneration}.`;

export const toSourceFacts = (
  source: NonNullable<EvidencePageView['source']>,
  now: number,
): LabelledValue[] => [
  { label: 'Process', value: toDisplayText(source.processInstance) },
  { label: 'Build', value: toDisplayText(source.build) },
  { label: 'Producer', value: toDisplayText(source.producer) },
  {
    label: 'Started',
    value: source.startedAt
      ? `${toLocalTime(source.startedAt)} (${toAgeAgo(source.startedAt, now) ?? absentText})`
      : absentText,
  },
];

export const toRawItemTitle = (type: string | undefined): string =>
  `evidence item${separator}${type || absentText}`;

export const toSupervisorFacts = ({ processID, build }: SupervisorEvidence): LabelledValue[] => [
  { label: 'Process', value: toDisplayText(processID) },
  { label: 'Build', value: toDisplayText(build) },
];

export const toUptimeLine = (
  { processStartedAt }: SupervisorEvidence,
  now: number,
): string | undefined => {
  const uptime = toElapsed(processStartedAt, now);
  return uptime === undefined
    ? undefined
    : `uptime ${uptime}${separator}started ${toLocalTime(processStartedAt)}`;
};

const toFlagText = (flag: Stated<boolean>, stated: string): string =>
  flag === true ? stated : flag === false ? `not ${stated}` : absentText;

export const toChildLine = (child: Stated<SupervisedChild>): string =>
  [
    child?.component || absentText,
    toFlagText(child?.desired, 'desired'),
    toFlagText(child?.running, 'running'),
    ...(child?.ownership ? [child.ownership] : []),
  ].join(separator);

const toObservationText = (
  { availability, sampledAt }: SupervisorObservation,
  now: number,
): string =>
  `${availability ?? absentText}${separator}sampled ${toAgeAgo(sampledAt, now) ?? absentText}`;

export const toRuntimeLine = (runtime: SupervisorObservation, now: number): string =>
  `runtime observation: ${toObservationText(runtime, now)}${runtime.reason ? `${separator}${toReasonText(runtime.reason)}` : ''}`;

export const toMirrorPodLine = (mirrorPod: SupervisorObservation, now: number): string =>
  `Kubernetes observation — mirror Pod: ${toObservationText(mirrorPod, now)}`;

export const toRoleCoverageText = ({
  expected,
  evaluated,
  unknown,
  notApplicable,
}: RoleCoverage): string =>
  [
    `expected ${expected}`,
    `evaluated ${evaluated}`,
    `unknown ${unknown}`,
    `not applicable ${notApplicable}`,
  ].join(separator);

const toReasonDetail = (reason: Stated<ReasonView>): string | undefined =>
  reason?.message && reason.code
    ? `${reason.code}: ${reason.message}`
    : reason?.message || reason?.code || undefined;

export const toCauseLine = (label: string, reason: Stated<ReasonView>): string => {
  const detail = toReasonDetail(reason);
  return detail === undefined ? label : `${label} — ${detail}`;
};

export const toExpiryBand = (notAfter: Stated<string>, now: number): string | undefined => {
  const expiresAt = fromTimestamp(notAfter);
  const remaining = (expiresAt ?? now) - now;
  return expiresAt === undefined
    ? undefined
    : remaining <= 0
      ? 'expired'
      : remaining < criticalExpiryWindowMs
        ? 'critical'
        : remaining < warningExpiryWindowMs
          ? 'warning'
          : 'healthy';
};

export const toExpiryTone = (notAfter: Stated<string>, now: number): Tone => {
  const band = toExpiryBand(notAfter, now);
  return toSeverityTone(band === 'expired' ? 'critical' : (band ?? ''));
};

export const toExpiryLine = (notAfter: Stated<string>, now: number): string | undefined => {
  const band = toExpiryBand(notAfter, now);
  const remaining = (fromTimestamp(notAfter) ?? now) - now;
  return band === undefined
    ? undefined
    : band === 'expired'
      ? `notAfter ${toLocalTime(notAfter)}${separator}expired`
      : `notAfter ${toLocalTime(notAfter)}${separator}${toRemaining(remaining)} left${separator}${band}`;
};

export const toSampleLine = (sampledAt: Stated<string>, now: number): string =>
  `sampled ${toLocalTime(sampledAt)}${separator}${toAgeAgo(sampledAt, now) ?? 'no sample age'}`;

export const toCertificateFacts = (
  evidence: CertificateEvidence | undefined,
  isReadStanding: boolean,
): LabelledValue[] => [
  { label: 'certificate', value: toDisplayText(evidence?.certificateID) },
  { label: 'role', value: toDisplayText(evidence?.role) },
  {
    label: 'fingerprint',
    value:
      evidence === undefined
        ? absentText
        : isReadStanding && evidence.fingerprint
          ? evidence.fingerprint
          : unknownCount,
  },
  { label: 'read state', value: toDisplayText(evidence?.readState) },
  { label: 'renewal owner', value: toDisplayText(evidence?.renewalOwner) },
];

export const toObservationFacts = ({
  availability,
  nativeOutcome,
}: ObservationView): LabelledValue[] => [
  { label: 'availability', value: toDisplayText(availability) },
  { label: 'outcome', value: toDisplayText(nativeOutcome) },
];

export const toRenewalFacts = (
  renewal: NonNullable<CertificateEvidence['renewal']>,
): LabelledValue[] => [
  { label: 'renewal outcome', value: toDisplayText(renewal.outcome) },
  { label: 'renewal attempted', value: toLocalTime(renewal.attemptedAt) },
  { label: 'renewal completed', value: toLocalTime(renewal.completedAt) },
  { label: 'renewal last success', value: toLocalTime(renewal.lastSuccessAt) },
  ...(renewal.operationID ? [{ label: 'renewal operation', value: renewal.operationID }] : []),
];

export const toVerdictWord = (outcome: Stated<string>): string =>
  !outcome || outcome === 'unknown' ? 'uncertain' : outcome;

const toRetentionText = (retention: BackupPolicy['retention']): string =>
  [
    ...(typeof retention?.maxAgeSeconds === 'number'
      ? [`keep ${toCoarseDuration(retention.maxAgeSeconds)}`]
      : []),
    ...(typeof retention?.maxDistinctBackups === 'number'
      ? [`${retention.maxDistinctBackups} distinct backups`]
      : []),
  ].join(separator) || unknownCount;

export const toPolicyLines = (policy: BackupEvidence['policy']): string[] =>
  !policy
    ? ['policy unknown']
    : [
        `state ${policy.state || unknownCount}`,
        `revision ${policy.revision ?? unknownCount}`,
        `provenance ${policy.provenance ?? unknownCount}`,
        policy.enabled === true
          ? 'enabled'
          : policy.enabled === false
            ? 'disabled'
            : 'enabled unknown',
        `required locations ${(policy.requiredLocations ?? []).join(', ') || 'none'}`,
        typeof policy.schedule?.expression === 'string'
          ? `schedule ${policy.schedule.expression}${policy.schedule.timezone ? ` (${policy.schedule.timezone})` : ''}`
          : 'schedule unknown',
        `retention ${toRetentionText(policy.retention)}`,
        `copy age bound ${typeof policy.maxAgeSeconds === 'number' ? toCoarseDuration(policy.maxAgeSeconds) : unknownCount}`,
      ];

export const toListingLine = ({
  complete,
  returnedDistinctBackups,
  totalDistinctBackups,
  reason,
}: BackupListing): string =>
  `listing ${complete === true ? 'complete' : 'partial'} — ${
    typeof returnedDistinctBackups === 'number' && typeof totalDistinctBackups === 'number'
      ? `${returnedDistinctBackups} of ${totalDistinctBackups} distinct backups served`
      : 'distinct backup counts unknown'
  }${reason?.message ? `${separator}${reason.message}` : ''}`;

export const toWarningText = (warning: Stated<ReasonView>): string =>
  warning?.message ?? unlabelledWarning;

export const toBackupCaption = ({ createdAt, sizeBytes }: DistinctBackup): string =>
  `created ${createdAt ?? unknownCount}${typeof sizeBytes === 'number' ? `${separator}${sizeBytes} bytes` : ''}`;

export const toCopyAge = (copy: DistinctCopy, now: number): string => {
  const success = copySuccessTimeOf(copy);
  return success === undefined
    ? 'never succeeded at its location'
    : isWithinSkew(success, now)
      ? `age ${toCoarseDuration((now - success) / 1000)} old`
      : 'age unknown — success timestamp exceeds allowed clock skew';
};

export const toAttemptLine = (
  label: string,
  { outcome, attemptedAt, reason }: BackupAttempt,
): string =>
  [
    `${label} ${outcome ?? unknownCount}`,
    ...(attemptedAt ? [`attempted ${attemptedAt}`] : []),
    ...(reason?.message ? [reason.message] : []),
  ].join(separator);
