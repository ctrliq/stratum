import { skipToken } from '@tanstack/react-query';
import { useContext } from 'react';

import type { ClusterFacts } from '~/api/fleet/fleetSchemas';
import { useGetCodeVersion } from '~/api/k8s/k8sComponents';
import type { Node } from '~/api/k8s/k8sSchemas';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { polledWhen } from '~/shared/query/polling';

import { isDriftReported } from '../utils/predicates';

export const useClusterDrift = (facts: ClusterFacts | undefined, nodes: Node[]): boolean => {
  const version = useGetCodeVersion(
    facts === undefined ? {} : skipToken,
    polledWhen(useContext(LiveRefreshContext)),
  );
  return isDriftReported(facts, version.data?.gitVersion, nodes);
};
