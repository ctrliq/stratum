import { useRouterState } from '@tanstack/react-router';

import { useListClusters } from '~/api/fleet/fleetComponents';
import { CapabilityAvailability } from '~/domain/clusters/components/CapabilityAvailability/CapabilityAvailability';
import { clusterWithId } from '~/domain/clusters/utils/selectors';
import { useScreenNavigation } from '~/domain/shell/hooks/useScreenNavigation';

import { EngineStateDrilldown } from '../../components/EngineStateDrilldown/EngineStateDrilldown';
import { toClusterListPath } from '../../utils/formatters';

type Props = {
  clusterId: string;
};

export function EngineStateDetail({ clusterId }: Props) {
  const { onOpened } = useScreenNavigation();
  const listSearch = useRouterState({ select: (state) => state.location.searchStr });
  const { data: cluster } = useListClusters(
    {},
    { select: ({ items }) => clusterWithId(items, clusterId) },
  );

  return (
    <EngineStateDrilldown
      clusterId={clusterId}
      clusterName={cluster?.name}
      onClosed={() => {
        onOpened(toClusterListPath(listSearch));
      }}
    >
      <CapabilityAvailability clusterId={clusterId} />
    </EngineStateDrilldown>
  );
}
