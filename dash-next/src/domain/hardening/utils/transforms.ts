import type { CisControl, ClusterFacts, Evidence } from '~/api/fleet/fleetSchemas';
import type { Lease, Namespace } from '~/api/k8s/k8sSchemas';
import { hasControlPlaneRole } from '~/domain/nodes/utils/predicates';
import type { ApiError } from '~/shared/fetchers/requests';
import { hasReason, hasStatus, isApiError } from '~/shared/fetchers/requests';
import { hasAnswered, type PolledRead } from '~/shared/query/loadState';
import { absentText, toErrorMessage } from '~/shared/utils/formatters';
import { isRecord } from '~/shared/utils/predicates';
import { nameOf } from '~/shared/utils/selectors';

import {
  backupReadings,
  certificateSignal,
  cleanAvailability,
  cleanReadState,
  cleanRenewalOutcome,
  clockSkewMs,
  failedMessage,
  leaseDurationWhenUnstated,
  notAnObjectMessage,
  pendingUnknown,
  retainedFromHeader,
  retainedToHeader,
  timedOutMessage,
  unknownCount,
} from '../constants';
import type {
  ControlGroup,
  OperatorVerdict,
  PendingRequests,
  Posture,
  PostureCard,
  PostureLoad,
  PostureReads,
  PostureSignal,
  RestrictedNamespaces,
  SigningRequest,
} from '../types';
import type {
  FindingFilters,
  OpenedEvidence,
  Refusal,
  RoleCoverage,
  Stated,
} from '../types/engineState';
import type {
  BackupAttempt,
  BackupCopy,
  BackupEvidence,
  BackupPolicy,
  BackupReading,
  BackupRecord,
  CertificateRow,
  DistinctBackup,
  DistinctCopy,
} from '../types/evidence';
import {
  toApprovedText,
  toCauseLine,
  toCoarseDuration,
  toOperatorDetail,
  toOperatorLabel,
} from './formatters';
import { fromTimestamp } from './parsers';
import {
  isAttemptFailed,
  isLaterAttempt,
  isLaterSuccess,
  isPendingKnown,
  isRestrictedNamespace,
  isWithinSkew,
} from './predicates';
import {
  approvedCountOf,
  copySuccessTimeOf,
  itemsOfType,
  observationKeyOf,
  observedAtOf,
  pendingCountOf,
} from './selectors';

export const toPendingRequests = (requests: SigningRequest[]): PendingRequests => ({
  pending: pendingCountOf(requests),
  availability: 'available',
});

export const fromRequestListError = (error: ApiError): PendingRequests => ({
  pending: undefined,
  availability: hasStatus(error, 403) ? 'forbidden' : 'unknown',
});

export const fromPendingRequestsFact = ({
  kubeletServingCSRPending,
  kubeletServingCSRAvailability,
}: Pick<
  ClusterFacts,
  'kubeletServingCSRPending' | 'kubeletServingCSRAvailability'
>): PendingRequests => ({
  pending: kubeletServingCSRPending ?? undefined,
  availability: kubeletServingCSRAvailability ?? 'unknown',
});

export const toRestrictedNamespaces = (
  namespaces: Pick<Namespace, 'metadata'>[] | undefined,
): RestrictedNamespaces => {
  const names = namespaces?.filter(isRestrictedNamespace).map(nameOf);
  return {
    names,
    restrictedCount: names?.length,
    namespaceCount: namespaces?.length,
  };
};

export const fromLeaseFact = (
  lease: Partial<ClusterFacts['operatorLease']> | undefined,
): OperatorVerdict => {
  const state = lease?.state || 'unknown';
  return {
    state,
    holder: state === 'missing' ? absentText : lease?.holder || absentText,
    isRefused: false,
  };
};

const toRenewedState = (renewedAt: number, durationSeconds: number, now: number): string =>
  renewedAt - now > clockSkewMs
    ? 'unknown'
    : now - renewedAt <= 2 * durationSeconds * 1000
      ? 'active'
      : 'inactive';

export const fromLease = ({ spec = {} }: Pick<Lease, 'spec'>, now: number): OperatorVerdict => {
  const renewedAt = fromTimestamp(spec.renewTime);
  const duration =
    spec.leaseDurationSeconds !== undefined && spec.leaseDurationSeconds > 0
      ? spec.leaseDurationSeconds
      : leaseDurationWhenUnstated;
  return {
    state:
      spec.holderIdentity && renewedAt !== undefined
        ? toRenewedState(renewedAt, duration, now)
        : 'inactive',
    holder: spec.holderIdentity || absentText,
    isRefused: false,
  };
};

export const fromLeaseError = (error: unknown): OperatorVerdict => ({
  state: hasStatus(error, 404) ? 'missing' : 'unknown',
  holder: absentText,
  isRefused: hasStatus(error, 403),
});

const withControl = (groups: ControlGroup[], control: CisControl): ControlGroup[] =>
  groups.some(({ group }) => group === control.group)
    ? groups.map((entry) =>
        entry.group === control.group
          ? {
              ...entry,
              sources:
                !control.source || entry.sources.includes(control.source)
                  ? entry.sources
                  : [...entry.sources, control.source],
              controls: [...entry.controls, control],
            }
          : entry,
      )
    : [
        ...groups,
        {
          group: control.group,
          sources: control.source ? [control.source] : [],
          controls: [control],
        },
      ];

export const toControlGroups = (profile: CisControl[]): ControlGroup[] =>
  profile.reduce<ControlGroup[]>(withControl, []);

const unreadRequests: PendingRequests = { pending: undefined, availability: 'unknown' };

export const toPosture = (
  { facts, lease, leaseFailure, requests, requestsFailure, namespaces, nodes }: PostureReads,
  now: number,
): Posture => ({
  operator:
    facts !== undefined
      ? fromLeaseFact(facts.operatorLease)
      : lease !== undefined
        ? fromLease(lease, now)
        : leaseFailure !== undefined
          ? fromLeaseError(leaseFailure)
          : fromLeaseFact(undefined),
  approved: requests === undefined ? undefined : approvedCountOf(requests),
  pending:
    facts !== undefined
      ? fromPendingRequestsFact(facts)
      : requests !== undefined
        ? toPendingRequests(requests)
        : isApiError(requestsFailure)
          ? fromRequestListError(requestsFailure)
          : unreadRequests,
  restricted: toRestrictedNamespaces(namespaces),
  controlPlaneCount: nodes.filter(hasControlPlaneRole).length,
  observedAt: observedAtOf(facts),
});

export const toPostureLoad = (nodesRead: PolledRead): PostureLoad =>
  hasAnswered(nodesRead) ? 'ready' : nodesRead.failureCount > 0 ? 'unreachable' : 'loading';

export const toPostureCards = ({
  operator,
  approved,
  pending,
  restricted,
  controlPlaneCount,
}: Posture): PostureCard[] => [
  {
    title: 'CSR approver',
    value: toOperatorLabel(operator.state),
    caption: operator.holder,
    isWarning: false,
  },
  {
    title: 'PodSecurity restricted',
    value:
      restricted.restrictedCount === undefined
        ? unknownCount
        : `${restricted.restrictedCount} / ${restricted.namespaceCount ?? absentText} ns`,
    caption:
      restricted.restrictedCount === undefined
        ? 'namespace read unavailable'
        : restricted.restrictedCount > 0
          ? 'enforce=restricted'
          : 'not enforced',
    isWarning: false,
  },
  {
    title: 'Kubelet serving certs',
    value: `${toApprovedText(approved)} approved`,
    caption: isPendingKnown(pending) ? `${pending.pending} pending` : pendingUnknown,
    isWarning: isPendingKnown(pending) && pending.pending !== 0,
  },
  {
    title: 'Control plane',
    value: `${controlPlaneCount} nodes`,
    caption: 'Stratum control-plane label',
    isWarning: false,
  },
];

export const toPostureSignals = ({
  operator,
  approved,
  pending,
  restricted,
  controlPlaneCount,
}: Posture): PostureSignal[] => [
  {
    label: 'Operator / CSR approver running',
    isOk: operator.state === 'active',
    detail: toOperatorDetail(operator),
  },
  isPendingKnown(pending)
    ? {
        label: 'No kubelet-serving CSR stuck pending',
        isOk: pending.pending === 0,
        detail: `${toApprovedText(approved)} approved · ${pending.pending} pending (kubelet-serving signer)`,
      }
    : {
        label: 'Kubelet-serving CSR pending state unknown',
        isOk: false,
        detail:
          pending.availability === 'forbidden'
            ? `${pendingUnknown} — the cluster credential cannot list requests`
            : `${pendingUnknown} — no usable observation`,
      },
  restricted.names === undefined
    ? {
        label: "PodSecurity 'restricted' state unknown",
        isOk: false,
        detail: 'PodSecurity restricted count unknown — the namespace read was unavailable',
      }
    : {
        label: "PodSecurity 'restricted' enforced",
        isOk: restricted.names.length > 0,
        detail:
          restricted.names.length > 0
            ? restricted.names.join(', ')
            : "no namespace enforces PodSecurity 'restricted'",
      },
  {
    label: 'Control-plane nodes labeled',
    isOk: controlPlaneCount > 0,
    detail: `${controlPlaneCount} node(s) carrying the Stratum control-plane label`,
  },
];

const answered = 200;

const toTransportMessage = (payload: unknown): string =>
  typeof payload === 'string' && payload.trim() !== '' ? payload.trim() : failedMessage;

export const toRefusal = (error: unknown): Refusal => {
  const body = isApiError(error) && isRecord(error.payload) ? error.payload : undefined;
  const status = isApiError(error) ? error.status : 'unknown';
  const headers = isApiError(error) ? (error.headers ?? {}) : {};
  return hasReason(error, 'timeout')
    ? { status, message: timedOutMessage, isRetryable: true }
    : hasReason(error, 'unreadable') && status === answered
      ? { status, message: notAnObjectMessage, isRetryable: false }
      : {
          status,
          message:
            isApiError(error) && status === 'unknown'
              ? toTransportMessage(error.payload)
              : toErrorMessage(error),
          isRetryable:
            body === undefined
              ? status === 429 || (status !== 'unknown' && status >= 500)
              : body.retryable === true,
          ...(typeof body?.error === 'string' ? { code: body.error } : {}),
          ...(typeof body?.requestID === 'string' ? { requestId: body.requestID } : {}),
          ...(typeof body?.currentReportGeneration === 'string'
            ? { currentReportGeneration: body.currentReportGeneration }
            : {}),
          ...(headers[retainedFromHeader] ? { retainedFrom: headers[retainedFromHeader] } : {}),
          ...(headers[retainedToHeader] ? { retainedTo: headers[retainedToHeader] } : {}),
        };
};

export const toFindingsQuery = (
  filters: FindingFilters,
  cursor?: string,
): Partial<FindingFilters> & { cursor?: string } =>
  Object.fromEntries(Object.entries({ ...filters, cursor }).filter(([, value]) => Boolean(value)));

export const toOptions = (values: readonly string[], allLabel: string): [string, string][] => [
  [allLabel, ''],
  ...values.map((value): [string, string] => [value, value]),
];

const toCount = (value: unknown): number =>
  typeof value === 'number' && Number.isFinite(value) ? value : 0;

const withRoleCounts = (roles: RoleCoverage[], { observation, evidence }: CertificateRow) => {
  const role = evidence?.role;
  const known = roles.find((entry) => entry.role === role) ?? {
    role: role ?? '',
    expected: 0,
    evaluated: 0,
    unknown: 0,
    notApplicable: 0,
  };
  const summed: RoleCoverage = {
    role: known.role,
    expected: known.expected + toCount(observation?.coverage?.expected),
    evaluated: known.evaluated + toCount(observation?.coverage?.evaluated),
    unknown: known.unknown + toCount(observation?.coverage?.unknown),
    notApplicable: known.notApplicable + toCount(observation?.coverage?.notApplicable),
  };
  return !role
    ? roles
    : roles.includes(known)
      ? roles.map((entry) => (entry === known ? summed : entry))
      : [...roles, summed];
};

export const toRoleCoverage = (rows: CertificateRow[]): RoleCoverage[] =>
  rows.reduce<RoleCoverage[]>(withRoleCounts, []);

export const toWalkedCertificateRows = (
  observations: OpenedEvidence['certificateObservations'],
): CertificateRow[] =>
  observations.map((observation) => ({
    observation,
    evidence: observation.evidence ?? undefined,
  }));

export const toCertificateRows = (
  { signal, observation: openedObservation, certificateObservations }: OpenedEvidence,
  items: Evidence[],
): CertificateRow[] => {
  const fragments = signal === certificateSignal ? itemsOfType(items, certificateSignal) : [];
  const openedKey =
    openedObservation === undefined || fragments.length === 0
      ? undefined
      : observationKeyOf(openedObservation);
  const isFilled = certificateObservations.some(
    (observation) => observationKeyOf(observation) === openedKey,
  );
  return signal === certificateSignal
    ? [
        ...certificateObservations.map((observation) => ({
          observation,
          evidence:
            observationKeyOf(observation) === openedKey
              ? fragments[0]
              : (observation.evidence ?? undefined),
        })),
        ...fragments
          .slice(isFilled ? 1 : 0)
          .map((evidence) => ({ observation: undefined, evidence })),
      ]
    : [];
};

export const toCertificateCauses = ({ observation, evidence }: CertificateRow): string[] => {
  const readState = evidence?.readState;
  const availability = observation?.availability;
  const renewal = evidence?.renewal;
  const readFacts = [
    ...(readState && readState !== cleanReadState ? [`read state ${readState}`] : []),
    ...(availability && availability !== cleanAvailability ? [`availability ${availability}`] : []),
  ];
  return [
    ...(readFacts.length > 0 ? [toCauseLine(readFacts.join(' · '), observation?.reason)] : []),
    ...(renewal?.outcome && renewal.outcome !== cleanRenewalOutcome
      ? [toCauseLine(`renewal ${renewal.outcome}`, renewal.reason)]
      : []),
  ];
};

export const toReconciledAttempt = (
  previous: Stated<BackupAttempt>,
  current: Stated<BackupAttempt>,
  now: number,
): BackupAttempt | undefined => {
  const [latest, other] =
    previous && current && isLaterAttempt(current, previous)
      ? [current, previous]
      : [previous, current];
  return !previous || !current
    ? (current ?? previous ?? undefined)
    : isLaterSuccess(fromTimestamp(other?.lastSuccessAt), fromTimestamp(latest?.lastSuccessAt), now)
      ? { ...latest, lastSuccessAt: other?.lastSuccessAt }
      : { ...latest };
};

const withCopy =
  (now: number) =>
  (copies: DistinctCopy[], copy: BackupCopy): DistinctCopy[] => {
    const { copyID } = copy;
    const key = `${copy.locationID ?? ''}\u0000${copyID ?? ''}`;
    return copyID === undefined || copyID === null
      ? copies
      : copies.some((known) => known.key === key)
        ? copies.map((known) =>
            known.key === key
              ? {
                  ...known,
                  save: toReconciledAttempt(known.save, copy.save, now),
                  upload: toReconciledAttempt(known.upload, copy.upload, now),
                }
              : known,
          )
        : [...copies, { ...copy, copyID, key }];
  };

const withRecord =
  (now: number) =>
  (backups: DistinctBackup[], record: BackupRecord): DistinctBackup[] => {
    const { backupID } = record;
    const copies = record.copies ?? [];
    return backupID === undefined || backupID === null
      ? backups
      : backups.some((known) => known.backupID === backupID)
        ? backups.map((known) =>
            known.backupID === backupID
              ? {
                  backupID,
                  createdAt: known.createdAt ?? record.createdAt,
                  sizeBytes: known.sizeBytes ?? record.sizeBytes,
                  copies: copies.reduce(withCopy(now), known.copies),
                }
              : known,
          )
        : [
            ...backups,
            {
              backupID,
              createdAt: record.createdAt,
              sizeBytes: record.sizeBytes,
              copies: copies.reduce(withCopy(now), []),
            },
          ];
  };

export const toDistinctBackups = (records: Stated<BackupRecord[]>, now: number): DistinctBackup[] =>
  (records ?? []).reduce(withRecord(now), []);

const toFailures = (backups: DistinctBackup[]): string[] =>
  backups.flatMap(({ backupID, copies }) =>
    copies.flatMap((copy) =>
      [
        { label: 'save', attempt: copy.save },
        { label: 'upload', attempt: copy.upload },
      ]
        .filter(({ attempt }) => isAttemptFailed(attempt))
        .map(
          ({ label }) =>
            `the ${label} attempt for copy ${copy.copyID} at ${copy.locationID ?? absentText} of ${backupID} failed`,
        ),
    ),
  );

const freshestSuccessAt = (
  backups: DistinctBackup[],
  location: string,
  now: number,
): number | undefined =>
  backups
    .flatMap(({ copies }) => copies)
    .filter((copy) => copy.locationID === location)
    .map(copySuccessTimeOf)
    .filter((time) => time !== undefined)
    .filter((time) => isWithinSkew(time, now))
    .reduce<number | undefined>(
      (freshest, time) => (freshest === undefined || time > freshest ? time : freshest),
      undefined,
    );

const toCoverageReading = (
  policy: BackupPolicy,
  maxAgeSeconds: number,
  backups: DistinctBackup[],
  now: number,
): BackupReading => {
  const locations = [...new Set(policy.requiredLocations ?? [])].map((location) => ({
    location,
    success: freshestSuccessAt(backups, location, now),
  }));
  const missing = locations
    .filter(({ success }) => success === undefined)
    .map(({ location }) => `no copy covers required location ${location}`);
  const stale = locations
    .map(({ location, success }) => ({
      location,
      age: success === undefined ? 0 : Math.max(0, Math.floor((now - success) / 1000)),
    }))
    .filter(({ age }) => age > maxAgeSeconds)
    .map(
      ({ location, age }) =>
        `the freshest copy at ${location} is ${toCoarseDuration(age)} old, past the configured ${toCoarseDuration(maxAgeSeconds)} bound`,
    );
  const problems = [...missing, ...stale, ...toFailures(backups)];
  return problems.length === 0
    ? { word: 'satisfactory', detail: backupReadings.satisfactory }
    : {
        word: missing.length > 0 || stale.length > 0 ? 'critical' : 'partial',
        detail: problems.join('; '),
      };
};

export const toBackupReading = (
  { policy, listing, backups }: BackupEvidence,
  now: number,
): BackupReading => {
  const state = policy?.state ?? '';
  const scheduleState = policy?.schedule?.state;
  const maxAgeSeconds = policy?.maxAgeSeconds;
  return state === 'unconfigured'
    ? { word: 'unconfigured', detail: backupReadings.unconfigured }
    : state === 'disabled' || policy?.enabled === false
      ? { word: 'disabled', detail: backupReadings.disabled }
      : !policy || state !== 'configured' || policy.enabled !== true
        ? { word: 'uncertain', detail: backupReadings.unknownPolicy }
        : scheduleState !== 'enabled' && scheduleState !== 'disabled'
          ? { word: 'uncertain', detail: backupReadings.unknownSchedule }
          : maxAgeSeconds === undefined || maxAgeSeconds === null
            ? { word: 'uncertain', detail: backupReadings.unknownAgeBound }
            : listing?.complete !== true
              ? { word: 'uncertain', detail: backupReadings.incompleteListing }
              : toCoverageReading(policy, maxAgeSeconds, toDistinctBackups(backups, now), now);
};

export const toWalkedRoleCoverage = (
  observations: OpenedEvidence['certificateObservations'],
): RoleCoverage[] | undefined =>
  observations.length === 0 ? undefined : toRoleCoverage(toWalkedCertificateRows(observations));
