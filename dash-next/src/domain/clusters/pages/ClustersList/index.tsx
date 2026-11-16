import { AlertType, ButtonVariant } from '@ctrliq/quantic-components';
import { Alert, Button, ButtonBar, ButtonBarItem, TextInput } from '@ctrliq/quantic-react';
import { useCallback, useContext, useState } from 'react';

import type { Cluster } from '~/api/fleet/fleetSchemas';
import { EvidenceArchiveFlows } from '~/domain/support/components/EvidenceArchiveFlows/EvidenceArchiveFlows';
import { EmptyState } from '~/shared/components/EmptyState/EmptyState';
import { PageBleed } from '~/shared/components/PageBleed/PageBleed';
import { PageTitle } from '~/shared/components/PageTitle/PageTitle';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { WriteAccessContext } from '~/shared/context/WriteAccessContext';
import { hasAnswered } from '~/shared/query/loadState';
import { toErrorMessage } from '~/shared/utils/formatters';
import { hasFeature } from '~/shared/utils/predicates';

import { noClustersRegistered, unreadFleet, unrefreshedFleet } from '../../constants';
import { useClusters } from '../../hooks/useClusters';
import { useClustersView } from '../../hooks/useClustersView';
import { CleanupCommandDialog } from '../../modals/CleanupCommandDialog';
import { DeployClusterDialog } from '../../modals/DeployClusterDialog';
import { DestroyClusterDialog } from '../../modals/DestroyClusterDialog';
import { RegisterClusterDialog } from '../../modals/RegisterClusterDialog';
import { UnregisterClusterDialog } from '../../modals/UnregisterClusterDialog';
import { matchingSearch } from '../../utils/filters';
import { toNoMatchText, toSummaryLine } from '../../utils/formatters';
import { healthyCountOf } from '../../utils/selectors';
import styles from './ClustersList.module.scss';
import { ClusterActions } from './parts/ClusterActions';
import { ClusterCards } from './parts/ClusterCards';
import { ClusterTable } from './parts/ClusterTable';
import { FleetEngineStateCard } from './parts/FleetEngineStateCard';

type OpenDialog =
  | { kind: 'register' }
  | { kind: 'deploy' }
  | { kind: 'unregister'; cluster: Cluster }
  | { kind: 'destroy'; cluster: Cluster }
  | { kind: 'cleanup'; clusterName: string; command: string };

type Props = {
  search: string;
  onSearchChanged: (search: string) => void;
};

export function ClustersList({ search, onSearchChanged }: Props) {
  const config = useServerConfig();
  const canManage = useContext(WriteAccessContext);
  const list = useClusters();
  const { view, onViewChosen } = useClustersView();
  const [query, setQuery] = useState(search);
  const [dialog, setDialog] = useState<OpenDialog>();
  const clusters = list.data ?? [];
  const shown = matchingSearch(clusters, query);
  const onDialogClosed = useCallback(() => {
    setDialog(undefined);
  }, []);
  const actionsFor = useCallback(
    (cluster: Cluster) => (
      <ClusterActions
        cluster={cluster}
        onUnregisterChosen={(chosen) => {
          setDialog({ kind: 'unregister', cluster: chosen });
        }}
        onDestroyChosen={(chosen) => {
          setDialog({ kind: 'destroy', cluster: chosen });
        }}
      />
    ),
    [],
  );
  return (
    <>
      <header className={styles.header}>
        <PageTitle className={styles.title}>Clusters</PageTitle>
        <p className={styles.summary}>{toSummaryLine(healthyCountOf(clusters), clusters.length)}</p>
        {canManage ? (
          <menu className={styles.actions}>
            <li>
              <Button
                variant={ButtonVariant.Outline}
                onClick={() => {
                  setDialog({ kind: 'register' });
                }}
              >
                Register existing
              </Button>
            </li>
            {hasFeature(config, 'capi') ? (
              <li>
                <Button
                  onClick={() => {
                    setDialog({ kind: 'deploy' });
                  }}
                >
                  Deploy cluster
                </Button>
              </li>
            ) : null}
          </menu>
        ) : null}
      </header>
      <FleetEngineStateCard />
      <search className={styles.toolbar}>
        <TextInput
          type="search"
          aria-label="Search clusters"
          placeholder="Search clusters…"
          value={query}
          onValueChanged={(event) => {
            setQuery(event.detail.value);
            onSearchChanged(event.detail.value);
          }}
        />
        <ButtonBar aria-label="View">
          <ButtonBarItem
            active={view === 'grid'}
            onClick={() => {
              onViewChosen('grid');
            }}
          >
            Grid
          </ButtonBarItem>
          <ButtonBarItem
            active={view === 'table'}
            onClick={() => {
              onViewChosen('table');
            }}
          >
            Table
          </ButtonBarItem>
        </ButtonBar>
      </search>
      {list.failureCount > 0 ? (
        <Alert type={AlertType.Error}>
          <span slot="title">{hasAnswered(list) ? unrefreshedFleet : unreadFleet}</span>
          {toErrorMessage(list.failureReason)}
        </Alert>
      ) : null}
      <EvidenceArchiveFlows clusters={clusters}>
        {shown.length > 0 ? (
          view === 'grid' ? (
            <ClusterCards clusters={shown} actionsFor={actionsFor} />
          ) : (
            <PageBleed>
              <ClusterTable clusters={shown} actionsFor={actionsFor} />
            </PageBleed>
          )
        ) : clusters.length > 0 ? (
          <EmptyState>{toNoMatchText(query)}</EmptyState>
        ) : hasAnswered(list) ? (
          <EmptyState>{noClustersRegistered}</EmptyState>
        ) : null}
      </EvidenceArchiveFlows>
      {dialog?.kind === 'register' ? (
        <RegisterClusterDialog clusters={clusters} onClosed={onDialogClosed} />
      ) : null}
      {dialog?.kind === 'deploy' ? (
        <DeployClusterDialog clusters={clusters} onClosed={onDialogClosed} />
      ) : null}
      {dialog?.kind === 'unregister' ? (
        <UnregisterClusterDialog
          cluster={dialog.cluster}
          onClosed={onDialogClosed}
          onCleanupOffered={(command) => {
            setDialog({ kind: 'cleanup', clusterName: dialog.cluster.name, command });
          }}
        />
      ) : null}
      {dialog?.kind === 'destroy' ? (
        <DestroyClusterDialog cluster={dialog.cluster} onClosed={onDialogClosed} />
      ) : null}
      {dialog?.kind === 'cleanup' ? (
        <CleanupCommandDialog
          clusterName={dialog.clusterName}
          command={dialog.command}
          onClosed={onDialogClosed}
        />
      ) : null}
    </>
  );
}
