import type {
  CronJob,
  DaemonSet,
  Deployment,
  Job,
  OwnerReference,
  Pod,
  StatefulSet,
} from '~/api/k8s/k8sSchemas';
import type { KubeObject } from '~/shared/types/kubernetes';
import { nameOf, namespaceOf } from '~/shared/utils/selectors';

import { degradedStatuses, finishedPhases } from '../constants';
import type { KindRead, PodStats, UsageByPod, UsageRead, WorkloadRow } from '../types/workloads';

export const isTerminating = ({ metadata }: Pod): boolean =>
  metadata?.deletionTimestamp !== undefined;

export const isUnschedulable = ({ status }: Pod): boolean =>
  (status?.conditions ?? []).some(
    (condition) =>
      condition.type === 'PodScheduled' &&
      condition.status === 'False' &&
      condition.reason === 'Unschedulable',
  );

export const isFinished = ({ status }: Pod): boolean =>
  finishedPhases.includes(status?.phase ?? '');

export const hasReportedUsage = ({ cpu, memory }: PodStats): boolean =>
  typeof cpu?.usageNanoCores === 'number' && typeof memory?.workingSetBytes === 'number';

export const isCompleteSample = (reads: UsageRead[]): boolean =>
  reads.every(
    ({ data, failureCount }) =>
      data !== undefined && failureCount === 0 && data.every(hasReportedUsage),
  );

export const isSameUsage = (first: UsageByPod, second: UsageByPod): boolean =>
  first === second ||
  (first.size === second.size &&
    [...first].every(([podKey, { millicores, mebibytes }]) => {
      const other = second.get(podKey);
      return other?.millicores === millicores && other.mebibytes === mebibytes;
    }));

const hasTrailingSegments = (name: string, prefix: string, segments: number): boolean =>
  name.startsWith(`${prefix}-`) && name.slice(prefix.length + 1).split('-').length === segments;

const ownersOf = ({ metadata }: Pod): OwnerReference[] => metadata?.ownerReferences ?? [];

const isOwnedPodOf = (pod: Pod, workload: KubeObject, ownerKind: string): boolean =>
  namespaceOf(pod) === namespaceOf(workload) &&
  (ownersOf(pod).length > 0
    ? ownersOf(pod).some((owner) => owner.kind === ownerKind && owner.name === nameOf(workload))
    : nameOf(pod) === nameOf(workload) || hasTrailingSegments(nameOf(pod), nameOf(workload), 1));

const isGrandchildPodOf = (pod: Pod, workload: KubeObject, ownerKind: string): boolean =>
  namespaceOf(pod) === namespaceOf(workload) &&
  (ownersOf(pod).length > 0
    ? ownersOf(pod).some(
        (owner) => owner.kind === ownerKind && hasTrailingSegments(owner.name, nameOf(workload), 1),
      )
    : hasTrailingSegments(nameOf(pod), nameOf(workload), 2));

export const isPodOfDeployment = (pod: Pod, deployment: Deployment): boolean =>
  isGrandchildPodOf(pod, deployment, 'ReplicaSet');

export const isPodOfStatefulSet = (pod: Pod, statefulSet: StatefulSet): boolean =>
  isOwnedPodOf(pod, statefulSet, 'StatefulSet');

export const isPodOfDaemonSet = (pod: Pod, daemonSet: DaemonSet): boolean =>
  isOwnedPodOf(pod, daemonSet, 'DaemonSet');

export const isPodOfJob = (pod: Pod, job: Job): boolean => isOwnedPodOf(pod, job, 'Job');

export const isPodOfCronJob = (pod: Pod, cronJob: CronJob): boolean =>
  isGrandchildPodOf(pod, cronJob, 'Job');

export const isDegraded = ({ status }: Pick<WorkloadRow, 'status'>): boolean =>
  degradedStatuses.includes(status);

export const matchesWorkloadSearch = (
  { name, namespace, kind, image }: Pick<WorkloadRow, 'name' | 'namespace' | 'kind' | 'image'>,
  query: string,
): boolean =>
  [name, namespace ?? '', kind, image].join(' ').toLowerCase().includes(query.trim().toLowerCase());

export const isUnreadable = ({ state, hasAnswered }: KindRead): boolean =>
  !hasAnswered && (state === 'failed' || state === 'forbidden');

export const isFirstLoad = (reads: KindRead[]): boolean =>
  reads.every(({ hasAnswered }) => !hasAnswered) && reads.some(({ state }) => state === 'loading');

export const isWhollyRead = (reads: KindRead[]): boolean =>
  reads.every(({ hasAnswered }) => hasAnswered);
