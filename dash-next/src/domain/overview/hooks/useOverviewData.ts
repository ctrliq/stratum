import { skipToken } from '@tanstack/react-query';
import { useContext } from 'react';

import { useListClusters } from '~/api/fleet/fleetComponents';
import {
  useGetCodeVersion,
  useListAppsV1DaemonSetForAllNamespaces,
  useListAppsV1DeploymentForAllNamespaces,
  useListAppsV1StatefulSetForAllNamespaces,
  useListBatchV1CronJobForAllNamespaces,
  useListBatchV1JobForAllNamespaces,
  useListCertificatesV1CertificateSigningRequest,
  useListCoreV1EventForAllNamespaces,
  useListStorageV1StorageClass,
  useReadCoordinationV1NamespacedLease,
} from '~/api/k8s/k8sComponents';
import { isDashboardServer } from '~/domain/auth/utils/predicates';
import { clusterWithId } from '~/domain/clusters/utils/selectors';
import { toEventRows } from '~/domain/events/utils/transforms';
import { approvedCountOf } from '~/domain/hardening/utils/selectors';
import { fromPendingRequestsFact } from '~/domain/hardening/utils/transforms';
import { useNodeData } from '~/domain/nodes/hooks/useNodeData';
import { readinessOf } from '~/domain/nodes/utils/selectors';
import type { UsageByPod } from '~/domain/workloads/types/workloads';
import { toWorkloadRows, toWorkloadRowsFromFacts } from '~/domain/workloads/utils/transforms';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { isApiError } from '~/shared/fetchers/requests';
import { polledWhen } from '~/shared/query/polling';

import { controlPlaneNamespace, operatorLeaseName } from '../constants';
import { cniNameOf, defaultStorageClassNameOf, kubernetesVersionOf } from '../utils/selectors';
import {
  fromLocalFacts,
  fromOperatorLease,
  fromOperatorLeaseError,
  fromOperatorLeaseFact,
  toControlPlanePodCounts,
} from '../utils/transforms';

const noItems: never[] = [];
const noUsage: UsageByPod = new Map();
const operatorLease = {
  pathParams: { namespace: controlPlaneNamespace, name: operatorLeaseName },
};

export const useOverviewData = () => {
  const clusterId = useContext(ClusterContext);
  const config = useServerConfig();
  const isHub = !isDashboardServer(config);
  const polled = polledWhen(useContext(LiveRefreshContext));
  const nodeData = useNodeData();
  const version = useGetCodeVersion({}, polled);
  const clusters = useListClusters(isHub ? {} : skipToken, polled);
  const events = useListCoreV1EventForAllNamespaces({}, polled);
  const deployments = useListAppsV1DeploymentForAllNamespaces({}, polled);
  const statefulSets = useListAppsV1StatefulSetForAllNamespaces({}, polled);
  const daemonSets = useListAppsV1DaemonSetForAllNamespaces({}, polled);
  const jobs = useListBatchV1JobForAllNamespaces({}, polled);
  const cronJobs = useListBatchV1CronJobForAllNamespaces({}, polled);
  const storageClasses = useListStorageV1StorageClass({}, polled);
  const lease = useReadCoordinationV1NamespacedLease(isHub ? skipToken : operatorLease, polled);
  const requests = useListCertificatesV1CertificateSigningRequest(isHub ? {} : skipToken, polled);
  const facts = nodeData.facts.data;
  const nodes = nodeData.nodeList.data?.items;
  const loadedVersion = version.data ? kubernetesVersionOf(version.data) : '';
  const workloads = {
    deployments: deployments.data?.items ?? noItems,
    statefulSets: statefulSets.data?.items ?? noItems,
    daemonSets: daemonSets.data?.items ?? noItems,
    jobs: jobs.data?.items ?? noItems,
    cronJobs: cronJobs.data?.items ?? noItems,
  };
  const cluster = isHub
    ? clusterWithId(clusters.data?.items ?? noItems, clusterId ?? '')
    : nodes === undefined
      ? undefined
      : fromLocalFacts({
          k8sVersion: loadedVersion,
          stratumVersion: config.version,
          nodeCount: nodes.length,
          readyNodes: nodes.filter((node) => readinessOf(node) === 'Ready').length,
        });

  return {
    isHub,
    cluster,
    facts,
    kubernetesVersion: loadedVersion || (cluster?.k8sVersion ?? ''),
    rows: nodeData.rows,
    hasRows: nodeData.hasRows && nodeData.rows.length > 0,
    events: toEventRows(events.data?.items ?? noItems, events.dataUpdatedAt),
    workloads:
      facts === undefined
        ? toWorkloadRows(workloads, nodeData.podList.data?.items, noUsage)
        : toWorkloadRowsFromFacts(workloads, facts, nodeData.podList.data?.items, noUsage),
    podCounts:
      nodeData.podList.data === undefined ? undefined : toControlPlanePodCounts(nodeData.pods),
    operator: isHub
      ? facts && fromOperatorLeaseFact(facts.operatorLease)
      : lease.data
        ? fromOperatorLease(lease.data, lease.dataUpdatedAt)
        : isApiError(lease.failureReason)
          ? fromOperatorLeaseError(lease.failureReason)
          : undefined,
    cni: cniNameOf(workloads.daemonSets),
    defaultStorageClass: defaultStorageClassNameOf(storageClasses.data?.items ?? noItems),
    signingRequests: facts && {
      ...fromPendingRequestsFact(facts),
      approved: requests.data ? approvedCountOf(requests.data.items) : undefined,
    },
  };
};
