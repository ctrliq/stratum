import type {
  Capability,
  Cluster,
  Coverage,
  FindingCounts,
  FleetSummary,
} from '~/api/fleet/fleetSchemas';
import { hasReason } from '~/shared/fetchers/requests';
import type { Tone } from '~/shared/types/tone';
import { absentText, toAgeAgo, toErrorMessage, toLocalTime } from '~/shared/utils/formatters';

import {
  engineStateTimedOut,
  fleetManagerClusterCode,
  fleetManagerKept,
  malformedName,
  malformedPlacement,
  malformedVersion,
  memberUnreachableCode,
  nameTakenCode,
  placementHelp,
  provisioningDisabled,
  provisioningStatus,
  takenName,
  versionHelp,
} from '../constants';
import type { FieldHint, NamedCluster } from '../types';
import {
  hasErrorCode,
  isClusterName,
  isKubernetesVersion,
  isNameTaken,
  isPlacementText,
  isProvisioningDisabled,
  isRetryable,
} from './predicates';

const separator = ' · ';

const severityTones: Record<string, Tone> = {
  healthy: 'success',
  info: 'progress',
  warning: 'warning',
  critical: 'error',
};

const freshnessTones: Record<string, Tone> = {
  fresh: 'success',
  stale: 'warning',
  warning: 'warning',
  degraded: 'error',
};

export const toContext = ({
  id,
  name,
  displayName,
  labels,
}: Pick<Cluster, 'id' | 'name' | 'displayName' | 'labels'>): string =>
  displayName && displayName !== name ? displayName : (Object.values(labels)[0] ?? (name || id));

export const toProviderText = ({ provider }: Pick<Cluster, 'provider'>): string =>
  provider || absentText;

export const toVersionText = (version: string): string =>
  version ? `v${version.replace(/^v/, '')}` : absentText;

export const toNodesText = ({
  readyNodes = 0,
  nodeCount = 0,
}: Partial<Pick<Cluster, 'readyNodes' | 'nodeCount'>>): string => `${readyNodes}/${nodeCount}`;

export const toSummaryLine = (healthy: number, total: number): string =>
  `${healthy} of ${total} healthy${separator}context: ${absentText}`;

export const toNoMatchText = (query: string): string => `No clusters match "${query}".`;

const toHint = (problem: string | undefined, help: string): FieldHint => ({
  text: problem ?? help,
  isError: problem !== undefined,
});

export const toNameHint = (name: string, clusters: NamedCluster[], help: string): FieldHint =>
  toHint(
    name !== '' && !isClusterName(name)
      ? malformedName
      : isNameTaken(clusters, name)
        ? takenName
        : undefined,
    help,
  );

export const toVersionHint = (version: string): FieldHint =>
  toHint(
    version.trim() !== '' && !isKubernetesVersion(version) ? malformedVersion : undefined,
    versionHelp,
  );

export const toPlacementHint = (text: string): FieldHint =>
  toHint(isPlacementText(text) ? undefined : malformedPlacement, placementHelp);

export const toTokenRefusal = (error: unknown, name: string): string =>
  hasErrorCode(error, nameTakenCode)
    ? `The name "${name}" is already reserved by a cluster or an outstanding token.`
    : toErrorMessage(error);

export const toKubeconfigRefusal = (error: unknown, name: string): string =>
  hasErrorCode(error, nameTakenCode)
    ? `The name "${name}" is already taken.`
    : hasErrorCode(error, memberUnreachableCode)
      ? `The fleet manager could not reach that cluster: ${toErrorMessage(error)}`
      : toErrorMessage(error);

export const toDeployRefusal = (error: unknown): string =>
  isProvisioningDisabled(error) ? provisioningDisabled : toErrorMessage(error);

export const toActionError = (error: unknown): string =>
  isProvisioningDisabled(error) ? provisioningDisabled : `Error: ${toErrorMessage(error)}`;

export const toUnregisterError = (error: unknown): string =>
  hasErrorCode(error, fleetManagerClusterCode)
    ? fleetManagerKept
    : `Error: ${toErrorMessage(error)}`;

export const toDestroyTooltip = ({
  fleetManager,
  provisioned,
  deletionProtected,
}: Pick<Cluster, 'fleetManager' | 'provisioned' | 'deletionProtected'>): string =>
  fleetManager
    ? 'Cannot destroy — this cluster runs the fleet manager'
    : !provisioned
      ? 'Cannot destroy — this cluster was registered, not built by the fleet manager'
      : deletionProtected
        ? 'Cannot destroy — deletion protection is on'
        : 'Destroy cluster (ClusterAPI)';

export const toProtectionTooltip = ({
  deletionProtected,
}: Pick<Cluster, 'deletionProtected'>): string =>
  deletionProtected ? 'Remove destroy protection' : 'Protect from destroy';

export const toProtectionToast = (name: string, isProtected: boolean): string =>
  isProtected ? `${name} is now protected from destroy` : `Removed destroy protection from ${name}`;

export const toDestroyFacts = ({
  provider,
  nodeCount,
  status,
}: Pick<Cluster, 'provider' | 'status'> & Partial<Pick<Cluster, 'nodeCount'>>): string =>
  `Provider: ${provider || absentText}${separator}Nodes: ${nodeCount ?? 0}${
    status === provisioningStatus ? ' (still being built — machines may not exist yet)' : ''
  }`;

export const toEvaluatedText = (timestamp: string | undefined | null, now: number): string =>
  `evaluated ${toAgeAgo(timestamp, now) ?? absentText}`;

export const toObservedText = (timestamp: string | undefined | null, now: number): string =>
  timestamp ? `${toLocalTime(timestamp)} (${toAgeAgo(timestamp, now) ?? absentText})` : absentText;

const toClusterNoun = (count: number | undefined): string => (count === 1 ? 'cluster' : 'clusters');

export const toClusterCountText = ({ clusterCount }: Partial<FleetSummary>): string =>
  `${clusterCount ?? absentText} ${toClusterNoun(clusterCount)}`;

export const toUnknownCoverageText = ({
  requiredUnknownClusters = 0,
}: Partial<FleetSummary>): string =>
  `${requiredUnknownClusters} ${toClusterNoun(requiredUnknownClusters)} with unknown required coverage`;

export const toCoverageText = ({ evaluated, expected }: Partial<Coverage> = {}): string =>
  `${evaluated ?? absentText}/${expected ?? absentText} evaluated`;

export const toFindingCountsText = ({
  total,
  required,
  optional,
}: Partial<FindingCounts> = {}): string =>
  [
    `${total ?? absentText} total`,
    `${required ?? absentText} required`,
    `${optional ?? absentText} optional`,
  ].join(separator);

export const toSeverityTone = (severity: string): Tone => severityTones[severity] ?? 'neutral';

export const toFreshnessTone = (freshness: string): Tone => freshnessTones[freshness] ?? 'neutral';

export const toQuotedRefusal = (lead: string, error: unknown): string =>
  `${lead} — ${toErrorMessage(error)}`;

export const toEngineStateRefusal = (lead: string, error: unknown): string =>
  hasReason(error, 'timeout')
    ? `${lead} — ${engineStateTimedOut}${separator}retryable`
    : toQuotedRefusal(lead, error);

export const toRefusalNote = (lead: string, error: unknown): string =>
  `${toQuotedRefusal(lead, error)}${isRetryable(error) ? `${separator}retryable` : ''}`;

export const toDenialText = ({ reason }: Pick<Capability, 'reason'>): string | undefined =>
  reason ? `${reason.code}: ${reason.message}` : undefined;

export const toHandlerText = ({
  handlerVersion,
}: Pick<Capability, 'handlerVersion'>): string | undefined =>
  handlerVersion ? `handler ${handlerVersion}` : undefined;
