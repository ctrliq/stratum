import type { ClusterFacts } from '~/api/fleet/fleetSchemas';
import type { Node } from '~/api/k8s/k8sSchemas';
import type { FindingView } from '~/domain/hardening/types/engineState';
import { hasAnswered, type PolledRead } from '~/shared/query/loadState';
import { absentText } from '~/shared/utils/formatters';
import { hasLabel } from '~/shared/utils/predicates';
import { annotationOf } from '~/shared/utils/selectors';

import {
  controlPlaneRoleLabel,
  masterRoleLabel,
  outcomePrefix,
  stratumControlPlaneLabel,
  stratumVersionAnnotation,
} from '../constants';
import type { NodeRow, PressureCondition, Version } from '../types/nodes';
import type { Cause, VersionRow } from '../types/versions';
import { toNormalVersion } from './formatters';
import { fromVersion } from './parsers';

export const hasControlPlaneRole = (node: Pick<Node, 'metadata'>): boolean =>
  hasLabel(node, controlPlaneRoleLabel) ||
  hasLabel(node, masterRoleLabel) ||
  hasLabel(node, stratumControlPlaneLabel, 'true');

export const isCordoned = ({ spec }: Node): boolean => spec?.unschedulable === true;

const hasSameMinor = (first: Version, second: Version): boolean =>
  first.major === second.major && first.minor === second.minor;

const isSameVersion = (first: Version, second: Version): boolean =>
  hasSameMinor(first, second) && first.patch === second.patch;

const stampedVersionOf = (node: Node): Version | undefined =>
  fromVersion(annotationOf(node, stratumVersionAnnotation));

const kubeletVersionOf = ({ status }: Node): Version | undefined =>
  fromVersion(status?.nodeInfo?.kubeletVersion);

const isDriftedFrom = (node: Node, serving: Version): boolean => {
  const stamped = stampedVersionOf(node);
  const kubelet = kubeletVersionOf(node);
  return (
    (stamped !== undefined && !isSameVersion(stamped, serving)) ||
    (kubelet !== undefined && !hasSameMinor(kubelet, serving))
  );
};

export const isClusterDrifted = (servingVersion: string | undefined, nodes: Node[]): boolean => {
  const serving = fromVersion(servingVersion);
  return serving !== undefined && nodes.some((node) => isDriftedFrom(node, serving));
};

export const isClusterDriftedInFacts = ({
  versionDrift,
}: Pick<ClusterFacts, 'versionDrift'>): boolean => versionDrift;

export const isDriftReported = (
  facts: Pick<ClusterFacts, 'versionDrift'> | undefined,
  servingVersion: string | undefined,
  nodes: Node[],
): boolean =>
  facts === undefined ? isClusterDrifted(servingVersion, nodes) : isClusterDriftedInFacts(facts);

export const hasRowsDue = (nodeList: PolledRead, podList: PolledRead): boolean =>
  hasAnswered(nodeList) && (hasAnswered(podList) || podList.failureCount > 0);

export const isAwaitingRows = (nodeList: PolledRead, podList: PolledRead): boolean =>
  !hasRowsDue(nodeList, podList) && (hasAnswered(nodeList) || nodeList.failureCount === 0);

export const isReadyRow = ({ readiness }: Pick<NodeRow, 'readiness'>): boolean =>
  readiness === 'Ready';

export const hasHostShell = (
  row: Pick<NodeRow, 'readiness'>,
  isNodeShellOffered: boolean,
): boolean => isNodeShellOffered && isReadyRow(row);

export const hasMeter = (row: Pick<NodeRow, 'readiness'>, percent: number): boolean =>
  isReadyRow(row) && percent > 0;

export const isStamped = ({ stratumVersion }: Pick<NodeRow, 'stratumVersion'>): boolean =>
  stratumVersion !== absentText;

export const isRowDrifted = ({
  stratumVersion,
  kubeletVersion,
}: Pick<NodeRow, 'stratumVersion' | 'kubeletVersion'>): boolean =>
  isStamped({ stratumVersion }) &&
  toNormalVersion(stratumVersion) !== toNormalVersion(kubeletVersion);

export const isReportedByNone = ({ unknownNodes, nodeCount }: PressureCondition): boolean =>
  unknownNodes.length === nodeCount;

export const isPolicyFinding = ({ origin }: Pick<FindingView, 'origin'>): boolean =>
  origin !== undefined && origin !== null;

export const isOutcomeCause = ({ code }: Pick<Cause, 'code'>): boolean =>
  (code ?? '').startsWith(outcomePrefix);

export const isIntentUnknown = ({ verdict, intent }: Pick<VersionRow, 'verdict' | 'intent'>) =>
  verdict === 'drift' && intent === undefined;
