import type { ClusterFacts } from '~/api/fleet/fleetSchemas';
import type { Node, Pod } from '~/api/k8s/k8sSchemas';
import type { FindingView, ObservationView } from '~/domain/hardening/types/engineState';
import { isEvidencePage } from '~/domain/hardening/utils/predicates';
import { itemsOf } from '~/domain/hardening/utils/selectors';
import type { NodeUsage } from '~/domain/overview/types';
import type { Tone } from '~/shared/types/tone';
import { absentText, toDisplayText } from '~/shared/utils/formatters';
import { isRecord } from '~/shared/utils/predicates';
import { annotationOf, creationTimeOf, labelOf, labelsOf, nameOf } from '~/shared/utils/selectors';

import {
  detailConditionTypes,
  explainedOutcome,
  pressureConditionTypes,
  rolloutDirectionUnknown,
  rolloutLabels,
  stratumVersionAnnotation,
  zoneLabel,
} from '../constants';
import type {
  ConditionRow,
  NodeFact,
  NodeRow,
  NodeStats,
  PressureBadge,
  PressureCondition,
} from '../types/nodes';
import type {
  Cause,
  Identity,
  IdentityValue,
  RolloutIntent,
  SampleSource,
  VersionLoad,
  VersionReason,
  VersionRow,
  VersionView,
} from '../types/versions';
import { podsOn } from './filters';
import {
  toAcceleratorsText,
  toCauseText,
  toIdentityText,
  toNodeCount,
  toReasonText,
  toServingDetail,
  toTaintText,
} from './formatters';
import { fromArtifact, fromIdentity, fromIntent, fromReason, fromSource } from './parsers';
import { isCordoned, isPolicyFinding, isReportedByNone } from './predicates';
import {
  acceleratorsOf,
  allocatableOf,
  conditionsOf,
  conditionStatusOf,
  evaluationKeyOf,
  evidenceRefOf,
  instanceTypeOf,
  podCapacityOf,
  readinessFromFacts,
  readinessOf,
  roleOf,
  usageOf,
  utilisationOf,
} from './selectors';

const millicoresPerCore = 1000;

const namesOf = (nodes: Node[]): string[] => nodes.map(nameOf);

const toPressureCondition = (type: string, nodes: Node[]): PressureCondition => ({
  type,
  trueNodes: namesOf(nodes.filter((node) => conditionStatusOf(node, type) === 'True')),
  unknownNodes: namesOf(
    nodes.filter((node) => [undefined, 'Unknown'].includes(conditionStatusOf(node, type))),
  ),
  nodeCount: nodes.length,
});

export const toPressureConditions = (nodes: Node[]): PressureCondition[] =>
  pressureConditionTypes.map((type) => toPressureCondition(type, nodes));

export const toNodeRow = (node: Node, stats: NodeStats | undefined, pods: Pod[]): NodeRow => ({
  ...utilisationOf(node, stats, pods),
  name: nameOf(node),
  createdAt: creationTimeOf(node),
  cpuCount: allocatableOf(node).millicores / millicoresPerCore,
  role: roleOf(node),
  readiness: readinessOf(node),
  isCordoned: isCordoned(node),
  podCount: podsOn(pods, node).length,
  podCapacity: podCapacityOf(node),
  kubeletVersion: toDisplayText(node.status?.nodeInfo?.kubeletVersion),
  stratumVersion: toDisplayText(annotationOf(node, stratumVersionAnnotation)),
  operatingSystem: toDisplayText(node.status?.nodeInfo?.osImage),
  containerRuntime: toDisplayText(node.status?.nodeInfo?.containerRuntimeVersion),
  kernel: toDisplayText(node.status?.nodeInfo?.kernelVersion),
  architecture: toDisplayText(node.status?.nodeInfo?.architecture),
  zone: toDisplayText(labelOf(node, zoneLabel)),
  instanceType: instanceTypeOf(node),
  taints: (node.spec?.taints ?? []).map(toTaintText),
  conditions: conditionsOf(node),
  labels: labelsOf(node),
  accelerators: acceleratorsOf(node),
});

export const toNodeRowFromFacts = (
  node: Node,
  facts: Pick<ClusterFacts, 'nodes'>,
  stats: NodeStats | undefined,
  pods: Pod[],
): NodeRow => ({
  ...toNodeRow(node, stats, pods),
  readiness: readinessFromFacts(node, facts),
});

export const toNodeUsages = (names: string[], stats: (NodeStats | undefined)[]): NodeUsage[] =>
  names.flatMap((nodeName, index) => {
    const measured = stats[index];
    const usage = measured === undefined ? undefined : usageOf(measured);
    return usage === undefined
      ? []
      : [{ nodeName, cpuMillicores: usage.millicores, memoryMebibytes: usage.mebibytes }];
  });

export const toPressureBadge = (condition: PressureCondition): PressureBadge =>
  condition.trueNodes.length > 0
    ? {
        text: toNodeCount(condition.trueNodes.length),
        tone: 'warning',
        detail: condition.trueNodes.join(', '),
      }
    : condition.unknownNodes.length === 0
      ? { text: 'False', tone: 'success' }
      : isReportedByNone(condition)
        ? { text: absentText, tone: 'neutral' }
        : { text: `${condition.unknownNodes.length} unknown`, tone: 'neutral' };

const toConditionTone = (type: string, status: string | undefined): Tone =>
  status === 'True' || status === 'False'
    ? (status === 'True') === (type === 'Ready')
      ? 'success'
      : 'warning'
    : 'neutral';

export const toConditionRows = ({ conditions }: Pick<NodeRow, 'conditions'>): ConditionRow[] =>
  detailConditionTypes.map((type) => ({
    type,
    text: toDisplayText(conditions[type]),
    tone: toConditionTone(type, conditions[type]),
  }));

export const toSystemFacts = (row: NodeRow): NodeFact[] => [
  { label: 'Instance type', value: row.instanceType },
  { label: 'Zone', value: row.zone },
  { label: 'Kubelet', value: row.kubeletVersion },
  { label: 'OS image', value: row.operatingSystem },
  { label: 'Stratum k8s-version', value: row.stratumVersion },
  { label: 'Container runtime', value: row.containerRuntime },
  { label: 'Kernel', value: row.kernel },
  { label: 'Architecture', value: row.architecture },
  { label: 'Accelerators', value: toAcceleratorsText(row.accelerators) },
];

const toCause = (finding: FindingView): Cause => ({
  code: finding.code ?? undefined,
  severity: finding.severity ?? undefined,
  message: finding.message ?? undefined,
  sampledAt: finding.sampledAt ?? undefined,
  isPolicy: isPolicyFinding(finding),
});

const toServingLabel = (
  isServingApplicable: boolean | undefined,
  serving: Identity | undefined,
): VersionRow['servingLabel'] =>
  isServingApplicable === false && serving === undefined
    ? 'Installed'
    : isServingApplicable !== false && serving?.raw !== undefined
      ? 'Running'
      : undefined;

export const toEvidenceByRef = (
  evidenceRefs: string[],
  pages: unknown[],
): VersionLoad['evidenceByRef'] =>
  Object.fromEntries(
    pages.flatMap((page, index) =>
      isEvidencePage(page) ? [[evidenceRefs[index] ?? '', itemsOf(page)]] : [],
    ),
  );

const toVersionRow = (
  key: string,
  findings: FindingView[],
  observation: ObservationView | undefined,
  evidenceByRef: VersionLoad['evidenceByRef'],
): VersionRow => {
  const outcome = findings.find((finding) => !isPolicyFinding(finding));
  const graded = outcome ?? findings[0];
  const evidenceRef = evidenceRefOf(findings);
  const subject = observation?.subject ?? graded?.subject;
  const found = observation?.evidence ?? evidenceByRef[evidenceRef ?? '']?.[0];
  const evidence = isRecord(found) ? found : {};
  const comparison = isRecord(evidence.comparison) ? evidence.comparison : {};
  const intent = fromIntent(evidence.intent);
  const serving = fromIdentity(evidence.serving);
  const isExplained =
    outcome?.code === explainedOutcome &&
    !findings.some(isPolicyFinding) &&
    intent?.phase !== undefined;
  return {
    key,
    nodeUID: subject?.nodeUID ?? undefined,
    component: subject?.component ?? undefined,
    verdict: typeof comparison.verdict === 'string' ? comparison.verdict : undefined,
    rule: typeof comparison.rule === 'string' ? comparison.rule : undefined,
    comparisonReason: fromReason(comparison.reason),
    observationReason: fromReason(observation?.reason),
    baselineReason: fromReason(evidence.baselineReason),
    expected: fromIdentity(evidence.expected),
    installed: fromIdentity(evidence.installed),
    serving,
    servingLabel: isRecord(found)
      ? toServingLabel(
          typeof evidence.servingApplicable === 'boolean' ? evidence.servingApplicable : undefined,
          serving,
        )
      : undefined,
    memberRelease: fromArtifact(evidence.memberRelease),
    intent,
    intentLabel: isExplained ? intent.phase : undefined,
    rolloutLabel: isExplained
      ? (rolloutLabels[intent.action ?? ''] ?? rolloutDirectionUnknown)
      : undefined,
    severity: graded?.severity || 'unknown',
    causes: findings.map(toCause),
    sampledAt: observation?.times?.sampledAt ?? undefined,
    source: fromSource(observation?.source),
    evidenceRef,
  };
};

const keyOf = ({ subject, signal }: Pick<FindingView, 'subject' | 'signal'>): string | undefined =>
  evaluationKeyOf(subject, signal);

const keysOf = (findings: FindingView[]): string[] => [
  ...new Set(findings.flatMap((finding) => keyOf(finding) ?? [])),
];

export const toVersionView = ({ summary, detail, evidenceByRef }: VersionLoad): VersionView => {
  const findings = detail?.findings ?? [];
  const observations = detail?.observations ?? [];
  return summary === undefined
    ? { isReachable: false, rows: [] }
    : {
        isReachable: true,
        cluster: summary.cluster,
        reportGeneration: detail?.reportGeneration ?? summary.reportGeneration,
        severity: summary.severity,
        freshness: summary.freshness,
        observedAt: summary.observedAt ?? undefined,
        coverage: summary.coverage,
        isTruncated: detail?.isCut === true,
        isPartial:
          detail !== undefined &&
          ((!detail.isComplete && !detail.isCut) || detail.state === 'partial'),
        rows: keysOf(findings).map((key) =>
          toVersionRow(
            key,
            findings.filter((finding) => keyOf(finding) === key),
            observations.find((observation) => keyOf(observation) === key),
            evidenceByRef,
          ),
        ),
      };
};

const toIdentityFacts = (label: string, identity: IdentityValue | undefined): NodeFact[] =>
  identity === undefined
    ? []
    : [
        ...(identity.digest ? [{ label: `${label} digest`, value: identity.digest }] : []),
        { label: `${label} availability`, value: identity.availability ?? 'unknown' },
        ...(identity.reason
          ? [{ label: `${label} reason`, value: toReasonText(identity.reason) }]
          : []),
      ];

const toIntentFacts = (intent: RolloutIntent | undefined): NodeFact[] =>
  intent === undefined
    ? [
        {
          label: 'Rollout intent',
          value: 'unknown — no rollout operation was reported for this component',
        },
      ]
    : [
        { label: 'Rollout operation', value: toDisplayText(intent.operationID) },
        { label: 'Operation target', value: toDisplayText(intent.targetUID) },
        { label: 'Operation action', value: intent.action ?? 'not stated — direction unknown' },
        { label: 'Operation phase', value: toDisplayText(intent.phase) },
        { label: 'Intended release', value: toDisplayText(intent.targetRelease?.releaseID) },
        ...(intent.targetRelease?.digest
          ? [{ label: 'Intended release digest', value: intent.targetRelease.digest }]
          : []),
        { label: 'Intent observed at', value: toDisplayText(intent.observedAt) },
        { label: 'Intent deadline', value: toDisplayText(intent.deadline) },
      ];

const toSourceFacts = (source: SampleSource | undefined): NodeFact[] =>
  source === undefined
    ? []
    : [
        {
          label: 'Sampled by',
          value: `${toDisplayText(source.producer)} build ${toDisplayText(source.build)}`,
        },
        { label: 'Writer binding', value: toDisplayText(source.bindingUID) },
        { label: 'Publisher member', value: toDisplayText(source.memberUID) },
        { label: 'Publisher node', value: toDisplayText(source.nodeUID) },
        { label: 'Process instance', value: toDisplayText(source.processInstance) },
        { label: 'Process epoch', value: toDisplayText(source.processEpoch) },
        { label: 'Source sequence', value: toDisplayText(source.sequence) },
        { label: 'Process started at', value: toDisplayText(source.startedAt) },
      ];

const toReasonFacts = (label: string, reason: VersionReason | undefined): NodeFact[] =>
  reason?.message || reason?.code
    ? [{ label, value: reason.message || reason.code || absentText }]
    : [];

export const toVersionFacts = (row: VersionRow, reportGeneration: string): NodeFact[] => [
  { label: 'Node', value: toDisplayText(row.nodeUID) },
  { label: 'Component', value: toDisplayText(row.component) },
  ...row.causes.map((cause) => ({ label: 'Hub finding', value: toCauseText(cause) })),
  ...(row.observationReason
    ? [{ label: 'Observation reason', value: toReasonText(row.observationReason) }]
    : []),
  { label: 'Comparison rule', value: toDisplayText(row.rule) },
  { label: 'Verdict', value: toDisplayText(row.verdict) },
  ...toReasonFacts('Comparison reason', row.comparisonReason),
  ...toReasonFacts('Baseline reason', row.baselineReason),
  {
    label: 'Member release baseline',
    value: row.memberRelease ? toDisplayText(row.memberRelease.releaseID) : 'none recorded',
  },
  ...(row.memberRelease?.digest
    ? [{ label: 'Member release digest', value: row.memberRelease.digest }]
    : []),
  { label: 'Expected', value: toIdentityText(row.expected) },
  ...toIdentityFacts('Expected', row.expected),
  { label: 'Installed', value: toIdentityText(row.installed) },
  ...toIdentityFacts('Installed', row.installed),
  { label: 'Serving', value: toServingDetail(row) },
  ...toIdentityFacts('Serving', row.serving),
  ...(row.serving?.instances ?? []).flatMap((instance, index) => [
    { label: `Serving instance ${index + 1}`, value: toIdentityText(instance) },
    ...toIdentityFacts(`Serving instance ${index + 1}`, instance),
  ]),
  { label: 'Report generation', value: toDisplayText(reportGeneration) },
  { label: 'Sampled at', value: toDisplayText(row.sampledAt) },
  ...toIntentFacts(row.intent),
  ...toSourceFacts(row.source),
];
