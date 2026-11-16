import { useServerConfig } from '~/shared/context/ConfigContext';

import { fleetPath } from '../../constants';
import { isFleetMode } from '../../utils/predicates';
import { NotFound } from './NotFound';

type Props = {
  clusterId: string;
};

export const ClusterNotFound = ({ clusterId }: Props) => {
  const isFleet = isFleetMode(useServerConfig());
  return (
    <NotFound
      title="Cluster not found"
      detail={`No cluster named "${clusterId}" is known to this dashboard.`}
      homeHref={isFleet ? fleetPath : '/'}
      homeLabel={isFleet ? 'Open the Clusters page' : 'Open the default cluster'}
    />
  );
};
