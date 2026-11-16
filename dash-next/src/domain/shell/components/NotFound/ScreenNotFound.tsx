import { useContext } from 'react';

import { ClusterContext } from '~/shared/context/ClusterContext';

import { overviewSegment } from '../../constants';
import { useScreenPlace } from '../../hooks/useScreenPlace';
import { toClusterPath } from '../../utils/transforms';
import { NotFound } from './NotFound';

export const ScreenNotFound = () => {
  const clusterId = useContext(ClusterContext) ?? '';
  const { location } = useScreenPlace();
  return (
    <NotFound
      title="Screen not found"
      detail={`"${location.segments.join('/')}" does not name a screen of the dashboard.`}
      homeHref={`${toClusterPath(clusterId)}/${overviewSegment}`}
      homeLabel="Open the cluster overview"
    />
  );
};
