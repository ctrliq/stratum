import { ButtonGrow, ButtonVariant } from '@ctrliq/quantic-components';
import { Button, Tooltip } from '@ctrliq/quantic-react';

import { fleetPath, noClusterCaption } from '../../constants';
import { useScreenNavigation } from '../../hooks/useScreenNavigation';
import type { ClusterIdentity } from '../../types/screen';
import { toClusterCaption, toSwitcherHint } from '../../utils/formatters';
import styles from './ClusterBlock.module.scss';

type Props = {
  cluster: ClusterIdentity | undefined;
};

type NameProps = Props & {
  fallback: string;
  captionFallback?: string;
};

const ClusterName = ({ cluster, fallback, captionFallback }: NameProps) => (
  <>
    {cluster ? (
      <small className={styles.caption}>{toClusterCaption(cluster)}</small>
    ) : captionFallback ? (
      <small className={styles.caption}>{captionFallback}</small>
    ) : null}
    <strong className={styles.name}>{cluster?.name ?? fallback}</strong>
  </>
);

export const ClusterLabel = ({ cluster }: Props) => (
  <p slot="header" className={styles.identity}>
    <ClusterName cluster={cluster} fallback="Cluster" />
  </p>
);

export const ClusterSwitcher = ({ cluster }: Props) => {
  const { onLinkClicked } = useScreenNavigation();
  return (
    <Tooltip slot="header">
      <Button
        as="a"
        href={fleetPath}
        variant={ButtonVariant.Ghost}
        grow={ButtonGrow.Always}
        onClick={(event) => {
          onLinkClicked(event, fleetPath);
        }}
      >
        <span className={styles.identity}>
          <ClusterName cluster={cluster} fallback="No cluster" captionFallback={noClusterCaption} />
        </span>
      </Button>
      <span slot="content">{toSwitcherHint(cluster)}</span>
    </Tooltip>
  );
};
