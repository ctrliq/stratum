import { useContext } from 'react';

import { useListClusters } from '~/api/fleet/fleetComponents';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { polledWhen } from '~/shared/query/polling';

import { clustersIn } from '../utils/selectors';

export const useClusters = () =>
  useListClusters({}, { ...polledWhen(useContext(LiveRefreshContext)), select: clustersIn });
