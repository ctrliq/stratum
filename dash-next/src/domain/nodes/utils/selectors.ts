import type { ClusterFacts } from '~/api/fleet/fleetSchemas';
import type { Node, Pod } from '~/api/k8s/k8sSchemas';
import type { FindingsWalk, FindingView } from '~/domain/hardening/types/engineState';
import type { ResourceAmounts } from '~/domain/workloads/types/workloads';
import { requestsOf, sumOf } from '~/domain/workloads/utils/selectors';
import type { Tone } from '~/shared/types/tone';
import { toDisplayText, toMebibytes, toMillicores } from '~/shared/utils/formatters';
import { fromCpuQuantity, fromMemoryQuantity } from '~/shared/utils/parsers';
import { labelOf, nameOf } from '~/shared/utils/selectors';

import {
  betaInstanceTypeLabel,
  defaultPodCapacity,
  instanceTypeLabel,
  tonesByStatus,
} from '../constants';
import type {
  Accelerator,
  NodeReadiness,
  NodeRole,
  NodeRow,
  NodeStats,
  NodeStatus,
  Utilisation,
} from '../types/nodes';
import type { VersionRow } from '../types/versions';
import { podsOn } from './filters';
import { hasControlPlaneRole } from './predicates';

export const roleOf = (node: Node): NodeRole =>
  hasControlPlaneRole(node) ? 'control-plane' : 'worker';

export const conditionStatusOf = ({ status }: Node, type: string): string | undefined =>
  (status?.conditions ?? []).find((condition) => condition.type === type)?.status;

export const conditionsOf = ({ status }: Node): Record<string, string> =>
  Object.fromEntries(
    (status?.conditions ?? []).map((condition) => [condition.type, condition.status]),
  );

const readyConditionOf = (node: Node): boolean | undefined => {
  const status = conditionStatusOf(node, 'Ready');
  return status === 'True' ? true : status === 'False' ? false : undefined;
};

const toReadiness = (readyCondition: boolean | undefined): NodeReadiness =>
  readyCondition === undefined ? 'Unknown' : readyCondition ? 'Ready' : 'NotReady';

export const readinessOf = (node: Node): NodeReadiness => toReadiness(readyConditionOf(node));

export const readinessFromFacts = (
  node: Node,
  { nodes }: Pick<ClusterFacts, 'nodes'>,
): NodeReadiness => toReadiness(nodes.find((fact) => fact.name === nameOf(node))?.readyCondition);

export const allocatableOf = ({ status }: Node): ResourceAmounts => ({
  millicores: fromCpuQuantity(status?.allocatable?.cpu),
  mebibytes: fromMemoryQuantity(status?.allocatable?.memory),
});

export const usageOf = ({ cpu, memory }: NodeStats): ResourceAmounts => ({
  millicores: toMillicores(cpu?.usageNanoCores ?? 0),
  mebibytes: toMebibytes(memory?.workingSetBytes ?? 0),
});

export const requestsOn = (node: Node, pods: Pod[]): ResourceAmounts =>
  sumOf(podsOn(pods, node).map(requestsOf));

const toPercent = (used: number, available: number): number =>
  available > 0 ? Math.min(100, Math.round((used / available) * 100)) : 0;

export const utilisationOf = (
  node: Node,
  stats: NodeStats | undefined,
  pods: Pod[],
): Utilisation => {
  const allocatable = allocatableOf(node);
  const used = stats === undefined ? requestsOn(node, pods) : usageOf(stats);
  return {
    cpuPercent: toPercent(used.millicores, allocatable.millicores),
    memoryPercent: toPercent(used.mebibytes, allocatable.mebibytes),
  };
};

export const podCapacityOf = ({ status }: Node): number =>
  Number(status?.allocatable?.pods ?? defaultPodCapacity);

export const instanceTypeOf = (node: Node): string =>
  toDisplayText(labelOf(node, instanceTypeLabel) ?? labelOf(node, betaInstanceTypeLabel));

export const acceleratorsOf = ({ status }: Node): Accelerator[] =>
  Object.entries(status?.capacity ?? {})
    .filter(([name]) => name.includes('/'))
    .map(([name, count]) => ({ name, count: String(count) }));

export const statusOf = ({
  readiness,
  isCordoned,
}: Pick<NodeRow, 'readiness' | 'isCordoned'>): NodeStatus =>
  readiness === 'Ready' && isCordoned ? 'Cordoned' : readiness;

export const toneOf = (row: Pick<NodeRow, 'readiness' | 'isCordoned'>): Tone =>
  tonesByStatus[statusOf(row)];

export const shownPercentOf = (row: Pick<NodeRow, 'readiness'>, percent: number): number =>
  row.readiness === 'Ready' ? percent : 0;

export const rowNamed = <TRow extends Pick<NodeRow, 'name'>>(
  rows: TRow[],
  name: string | undefined,
): TRow | undefined => rows.find((row) => row.name === name);

export const evaluationKeyOf = (
  subject: FindingView['subject'],
  signal: string | null | undefined,
): string | undefined =>
  subject
    ? JSON.stringify([
        subject.scope ?? null,
        subject.nodeUID ?? null,
        subject.component ?? null,
        subject.key ?? null,
        signal ?? null,
      ])
    : undefined;

export const versionRowKeyed = (
  rows: VersionRow[],
  key: string | undefined,
): VersionRow | undefined => rows.find((row) => row.key === key);

export const evidenceRefOf = (findings: Pick<FindingView, 'evidenceRef'>[]): string | undefined =>
  findings.find(({ evidenceRef }) => Boolean(evidenceRef))?.evidenceRef ?? undefined;

export const unheldEvidenceRefsOf = ({
  findings,
  observations,
}: Pick<FindingsWalk, 'findings' | 'observations'>): string[] => {
  const held = new Set(observations.map(({ subject, signal }) => evaluationKeyOf(subject, signal)));
  return [
    ...new Set(
      findings
        .filter(({ subject, signal }) => !held.has(evaluationKeyOf(subject, signal)))
        .flatMap(({ evidenceRef }) => evidenceRef || []),
    ),
  ];
};

export const firstRefusalOf = (errors: unknown[]): unknown =>
  errors.find((error) => error !== null && error !== undefined);
