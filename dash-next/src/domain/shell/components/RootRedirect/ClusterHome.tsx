import { useRouter } from '@tanstack/react-router';
import { useEffect } from 'react';

import { overviewSegment } from '../../constants';
import { useScreenPlace } from '../../hooks/useScreenPlace';
import { toClusterPath } from '../../utils/transforms';

export const ClusterHome = () => {
  const router = useRouter();
  const { location, searchStr } = useScreenPlace();
  const clusterId = location.clusterId;

  useEffect(() => {
    if (clusterId !== undefined) {
      router.history.replace(`${toClusterPath(clusterId)}/${overviewSegment}${searchStr}`);
    }
  }, [clusterId, searchStr, router]);

  return null;
};
