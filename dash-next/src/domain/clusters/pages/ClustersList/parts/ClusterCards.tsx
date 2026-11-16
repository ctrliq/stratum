import { Card, CardGroup, CardMeta, CardTitle } from '@ctrliq/quantic-react';
import type { ReactNode } from 'react';

import type { Cluster } from '~/api/fleet/fleetSchemas';
import { toClusterCaption } from '~/domain/shell/utils/formatters';
import { toDisplayText } from '~/shared/utils/formatters';

import { toNodesText, toVersionText } from '../../../utils/formatters';
import styles from './ClusterCards.module.scss';
import { ClusterName, ClusterStatus, EngineStateLink, UseCluster } from './ClusterEntry';

type Props = {
  clusters: Cluster[];
  actionsFor: (cluster: Cluster) => ReactNode;
};

export const ClusterCards = ({ clusters, actionsFor }: Props) => (
  <CardGroup>
    {clusters.map((cluster) => (
      <Card key={cluster.id}>
        <CardTitle slot="header">
          <ClusterName cluster={cluster} />
          <span slot="subtitle">{toClusterCaption(cluster)}</span>
        </CardTitle>
        <CardMeta slot="header">
          <ClusterStatus cluster={cluster} />
        </CardMeta>
        <dl className={styles.facts}>
          <dt>Region</dt>
          <dd>{toDisplayText(cluster.region)}</dd>
          <dt>Version</dt>
          <dd>{toVersionText(cluster.k8sVersion)}</dd>
          <dt>Nodes</dt>
          <dd>{toNodesText(cluster)}</dd>
        </dl>
        <footer slot="footer" className={styles.controls}>
          <UseCluster cluster={cluster} />
          <EngineStateLink cluster={cluster} />
          {actionsFor(cluster)}
        </footer>
      </Card>
    ))}
  </CardGroup>
);
