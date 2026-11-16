import { useRouterState } from '@tanstack/react-router';
import { useContext, useMemo } from 'react';

import { ClusterContext } from '~/shared/context/ClusterContext';

import { namespaceParam, tabParam } from '../constants';
import type { ScreenLocation } from '../types/screen';
import { fromPathname, fromSearch } from '../utils/parsers';

type ScreenPlace = {
  clusterId: string | undefined;
  namespace: string | undefined;
  tab: string | undefined;
  location: ScreenLocation;
  pathname: string;
  searchStr: string;
};

export const useScreenPlace = (): ScreenPlace => {
  const clusterId = useContext(ClusterContext);
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const searchStr = useRouterState({ select: (state) => state.location.searchStr });
  return useMemo(
    () => ({
      clusterId,
      namespace: fromSearch(searchStr, namespaceParam),
      tab: fromSearch(searchStr, tabParam),
      location: fromPathname(pathname),
      pathname,
      searchStr,
    }),
    [clusterId, pathname, searchStr],
  );
};
