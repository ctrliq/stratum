import { useContext } from 'react';

import { ClusterEngineState } from '~/domain/clusters/components/ClusterEngineState/ClusterEngineState';
import { localClusterId } from '~/domain/shell/constants';
import { useClusterReads } from '~/domain/shell/hooks/useClusterReads';
import { useManualRefresh } from '~/domain/shell/hooks/useManualRefresh';
import { useScreenNavigation } from '~/domain/shell/hooks/useScreenNavigation';
import { toClusterCaption } from '~/domain/shell/utils/formatters';
import { isClusterUnreachable, isFleetMode } from '~/domain/shell/utils/predicates';
import { refreshedAtOf } from '~/domain/shell/utils/selectors';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { NamespaceContext } from '~/shared/context/NamespaceContext';
import { absentText, toDisplayText, toLocalTime, toPercentText } from '~/shared/utils/formatters';

import { overviewNodeLimit } from '../../constants';
import { useOverviewData } from '../../hooks/useOverviewData';
import { toAsOf, toHeaderLine, toKubernetesText } from '../../utils/formatters';
import { regionOf, stratumVersionOf } from '../../utils/selectors';
import {
  toControlPlaneRows,
  toControlPlaneSummary,
  toHealthGroups,
  toNodesReadyTile,
  toPodsTile,
  toScreenLink,
  toSigningRequestsRow,
  toUtilisationTiles,
} from '../../utils/transforms';
import styles from './Overview.module.scss';
import { ClusterCard } from './parts/ClusterCard';
import { ControlPlaneCard } from './parts/ControlPlaneCard';
import { NodeUtilisation } from './parts/NodeUtilisation';
import { OverviewHeader } from './parts/OverviewHeader';
import { RecentEvents } from './parts/RecentEvents';
import { SummaryTile } from './parts/SummaryTile';
import { WorkloadHealth } from './parts/WorkloadHealth';

export function Overview() {
  const clusterId = useContext(ClusterContext);
  const namespace = useContext(NamespaceContext);
  const config = useServerConfig();
  const reads = useClusterReads();
  const onRefreshed = useManualRefresh();
  const { onLinkClicked } = useScreenNavigation();
  const data = useOverviewData();
  const { cluster, rows, facts } = data;
  const place = { clusterId, namespace };
  const name = cluster?.name ?? (data.isHub ? (clusterId ?? '') : localClusterId);
  const utilisation = toUtilisationTiles(rows);
  const pods = toPodsTile(rows);
  const nodesReady = toNodesReadyTile(cluster, data.hasRows);
  const controlPlane = toControlPlaneSummary(rows);
  const controlPlaneCount = rows.filter(({ role }) => role === 'control-plane').length;

  return (
    <section className={styles.main} aria-label="Overview">
      <OverviewHeader
        name={name}
        status={cluster?.status ?? 'pending'}
        line={toHeaderLine(
          cluster ? toClusterCaption(cluster) : [name, absentText].join(' · '),
          regionOf(
            cluster?.region,
            rows.map(({ zone }) => zone),
          ),
          data.kubernetesVersion,
        )}
        isFleetManager={isFleetMode(config) && cluster?.fleetManager === true}
        asOf={
          data.isHub && data.hasRows && isClusterUnreachable(reads)
            ? toAsOf(refreshedAtOf(reads))
            : undefined
        }
        onRefreshed={onRefreshed}
      />
      <div className={styles.tiles}>
        <SummaryTile label="Nodes Ready" value={nodesReady.value} caption={nodesReady.caption} />
        <SummaryTile
          label="CPU Utilization"
          value={toPercentText(utilisation.cpuPercent)}
          percent={utilisation.cpuPercent}
        />
        <SummaryTile
          label="Memory Utilization"
          value={toPercentText(utilisation.memoryPercent)}
          percent={utilisation.memoryPercent}
        />
        <SummaryTile label="Pods Scheduled" value={pods.value} caption={pods.caption} />
      </div>
      <div className={styles.cards}>
        <NodeUtilisation
          rows={rows.slice(0, overviewNodeLimit)}
          nodesHref={toScreenLink('nodes', place)}
          onLinkClicked={onLinkClicked}
        />
        <RecentEvents
          events={data.events}
          eventsHref={toScreenLink('events', place)}
          onLinkClicked={onLinkClicked}
        />
      </div>
      <div className={styles.cards}>
        {data.isHub ? <ClusterEngineState clusterId={clusterId} /> : null}
        <WorkloadHealth groups={toHealthGroups(data.workloads)} />
        <ControlPlaneCard
          summary={controlPlane}
          rows={toControlPlaneRows(data.podCounts, data.operator, controlPlaneCount)}
        />
        <ClusterCard
          rows={[
            { label: 'Kubernetes', value: toKubernetesText(data.kubernetesVersion) },
            {
              label: 'Stratum',
              value: stratumVersionOf(
                cluster?.stratumVersion,
                rows.map(({ kubeletVersion }) => kubeletVersion),
              ),
            },
            { label: 'Source', value: toDisplayText(cluster?.source) },
            { label: 'CNI', value: data.cni },
          ]}
          defaultStorageClass={data.defaultStorageClass}
          storageHref={toScreenLink('storage', place)}
          hubRows={
            data.isHub
              ? [
                  toSigningRequestsRow(data.signingRequests),
                  { label: 'Facts observed', value: toLocalTime(facts?.observedAt) },
                ]
              : []
          }
          onLinkClicked={onLinkClicked}
        />
      </div>
    </section>
  );
}
