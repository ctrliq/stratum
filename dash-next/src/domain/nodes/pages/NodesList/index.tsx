import { Alert, Button, LucideIconRefreshCw } from '@ctrliq/quantic-react';
import { useContext } from 'react';

import { isDashboardServer } from '~/domain/auth/utils/predicates';
import { loadingSummary } from '~/domain/resources/constants';
import { toListMessage, toStaleNotice } from '~/domain/resources/utils/formatters';
import { useManualRefresh } from '~/domain/shell/hooks/useManualRefresh';
import { useTerminal } from '~/domain/terminal/hooks/useTerminal';
import { PageBleed } from '~/shared/components/PageBleed/PageBleed';
import { PageTitle } from '~/shared/components/PageTitle/PageTitle';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { WriteAccessContext } from '~/shared/context/WriteAccessContext';
import { hasAnswered, toLoadState } from '~/shared/query/loadState';

import { clusterDriftHint, clusterDriftLabel, nodeKind } from '../../constants';
import { useClusterDrift } from '../../hooks/useClusterDrift';
import { useCordon } from '../../hooks/useCordon';
import { useNodeData } from '../../hooks/useNodeData';
import { useUsageTrend } from '../../hooks/useUsageTrend';
import { useVersionEvidence } from '../../hooks/useVersionEvidence';
import { driftedIn, podsScheduledOn } from '../../utils/filters';
import {
  toCpuCaption,
  toDriftSummary,
  toMemoryCaption,
  toSummaryLine,
} from '../../utils/formatters';
import { hasHostShell } from '../../utils/predicates';
import { rowNamed } from '../../utils/selectors';
import styles from './NodesList.module.scss';
import { ConditionsCard } from './parts/ConditionsCard';
import { NodeDetail } from './parts/NodeDetail';
import { NodeTable } from './parts/NodeTable';
import { UsageCard } from './parts/UsageCard';
import { VersionComparison } from './parts/versions/VersionComparison';
import { VersionNotice } from './parts/versions/VersionNotice';

type Props = {
  selectedName?: string;
  onSelected: (name: string | undefined) => void;
};

function NodesListIn({ selectedName, onSelected }: Props) {
  const config = useServerConfig();
  const canWrite = useContext(WriteAccessContext);
  const data = useNodeData();
  const usage = useUsageTrend(data.sample, data.sampledAt, data.isSampleSettled);
  const evidence = useVersionEvidence(!isDashboardServer(config));
  const onRefreshed = useManualRefresh();
  const { onCordonToggled } = useCordon();
  const { isNodeShellOffered, onNodeShellOpened } = useTerminal();
  const { rows, nodeList } = data;
  const isClusterDrifted = useClusterDrift(data.facts.data, data.sample.nodes);
  const state = toLoadState(nodeList);
  const drifted = driftedIn(rows);
  const selected = rowNamed(rows, selectedName);
  const notice = toStaleNotice(state, hasAnswered(nodeList));

  return (
    <section className={styles.main} aria-label="Nodes">
      <header className={styles.heading}>
        <hgroup>
          <PageTitle className={styles.title}>Nodes</PageTitle>
          <p className={styles.summary}>
            {data.isAwaitingRows ? loadingSummary : toSummaryLine(rows)}
            {drifted.length > 0 ? (
              <span className={styles.drift}> · {toDriftSummary(drifted.length)}</span>
            ) : null}
          </p>
          {isClusterDrifted ? (
            <p className={styles.verdict}>
              <ToneBadge tone="warning" hint={clusterDriftHint}>
                {clusterDriftLabel}
              </ToneBadge>
            </p>
          ) : null}
        </hgroup>
        <Button variant="outline" onClick={onRefreshed}>
          <LucideIconRefreshCw size="sm" />
          Refresh
        </Button>
      </header>
      {notice ? <Alert type="warning">{notice}</Alert> : null}
      <PageBleed className={styles.content}>
        <NodeTable
          rows={rows}
          readAt={nodeList.dataUpdatedAt}
          selectedName={selected?.name}
          message={
            data.isAwaitingRows
              ? undefined
              : toListMessage(state, hasAnswered(nodeList), rows.length, nodeKind)
          }
          isLoading={data.isAwaitingRows}
          canWrite={canWrite}
          isNodeShellOffered={isNodeShellOffered}
          onSelected={onSelected}
          onHostShellRequested={onNodeShellOpened}
          onCordonToggled={onCordonToggled}
        />
      </PageBleed>
      {selected ? (
        <NodeDetail
          row={selected}
          pods={podsScheduledOn(data.pods, selected.name)}
          readAt={nodeList.dataUpdatedAt}
          canWrite={canWrite}
          onHostShellRequested={
            hasHostShell(selected, isNodeShellOffered) ? onNodeShellOpened : undefined
          }
          onCordonToggled={onCordonToggled}
          onClosed={() => {
            onSelected(undefined);
          }}
        />
      ) : null}
      <div className={styles.cards}>
        <UsageCard
          title="Cluster CPU"
          percent={usage.totals.cpuPercent}
          caption={toCpuCaption(usage.totals)}
          samples={usage.history.cpu}
        />
        <UsageCard
          title="Cluster Memory"
          percent={usage.totals.memoryPercent}
          caption={toMemoryCaption(usage.totals)}
          samples={usage.history.memory}
        />
        <ConditionsCard conditions={data.conditions} />
      </div>
      {evidence.state === 'loaded' ? (
        <VersionComparison view={evidence.view} driftedRows={drifted} />
      ) : evidence.state === 'refused' ? (
        <VersionNotice reason={evidence.reason} driftedRows={drifted} />
      ) : null}
    </section>
  );
}

export function NodesList(props: Props) {
  const clusterId = useContext(ClusterContext);
  return <NodesListIn key={clusterId ?? ''} {...props} />;
}
