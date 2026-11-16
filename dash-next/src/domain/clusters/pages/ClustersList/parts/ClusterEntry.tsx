import { BadgeColor, BadgeSize, ButtonSize, ButtonVariant } from '@ctrliq/quantic-components';
import { Badge, Button } from '@ctrliq/quantic-react';
import { useRouterState } from '@tanstack/react-router';

import type { Cluster } from '~/api/fleet/fleetSchemas';
import { isUsable } from '~/domain/clusters/utils/predicates';
import { toEngineStatePath } from '~/domain/hardening/utils/formatters';
import { overviewSegment } from '~/domain/shell/constants';
import { useScreenNavigation } from '~/domain/shell/hooks/useScreenNavigation';
import { toClusterPath } from '~/domain/shell/utils/transforms';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';
import { toStatusLabel, toStatusTone } from '~/shared/utils/formatters';

type Props = {
  cluster: Cluster;
};

export const ClusterName = ({ cluster }: Props) => (
  <>
    {cluster.name}
    {cluster.fleetManager ? (
      <>
        {' '}
        <Badge color={BadgeColor.Purple} size={BadgeSize.Small}>
          Fleet manager
        </Badge>
      </>
    ) : null}
  </>
);

export const ClusterStatus = ({ cluster }: Props) => (
  <ToneBadge tone={toStatusTone(cluster.status)}>{toStatusLabel(cluster.status)}</ToneBadge>
);

export const UseCluster = ({ cluster }: Props) => {
  const { onOpened } = useScreenNavigation();
  return (
    <Button
      variant={ButtonVariant.Outline}
      size={ButtonSize.Small}
      disabled={!isUsable(cluster)}
      onClick={() => {
        onOpened(`${toClusterPath(cluster.id)}/${overviewSegment}`);
      }}
    >
      Use
    </Button>
  );
};

export const EngineStateLink = ({ cluster }: Props) => {
  const { onLinkClicked } = useScreenNavigation();
  const listSearch = useRouterState({ select: (state) => state.location.searchStr });
  const href = toEngineStatePath(cluster.id, listSearch);
  return (
    <Button
      as="a"
      href={href}
      variant={ButtonVariant.Ghost}
      size={ButtonSize.Small}
      onClick={(event) => {
        onLinkClicked(event, href);
      }}
    >
      Engine state
    </Button>
  );
};
