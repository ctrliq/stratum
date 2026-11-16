import { useQueryClient } from '@tanstack/react-query';
import { useCallback } from 'react';

import { getFleetEngineStateQuery, listClustersQuery } from '~/api/fleet/fleetComponents';

export const useFleetRefresh = () => {
  const queryClient = useQueryClient();
  return useCallback(
    () =>
      Promise.all(
        [listClustersQuery({}), getFleetEngineStateQuery({})].map(({ queryKey }) =>
          queryClient.invalidateQueries({ queryKey, exact: true }),
        ),
      ),
    [queryClient],
  );
};
