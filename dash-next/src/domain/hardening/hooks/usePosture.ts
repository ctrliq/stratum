import { skipToken } from '@tanstack/react-query';
import { useContext } from 'react';

import { useGetClusterFacts } from '~/api/fleet/fleetComponents';
import {
  useListCertificatesV1CertificateSigningRequest,
  useListCoreV1Namespace,
  useListCoreV1Node,
  useReadCoordinationV1NamespacedLease,
} from '~/api/k8s/k8sComponents';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { polledWhen } from '~/shared/query/polling';

import { operatorLeaseName, operatorLeaseNamespace } from '../constants';
import type { Posture, PostureLoad } from '../types';
import { toPosture, toPostureLoad } from '../utils/transforms';
import { useNow } from './useNow';

type PostureRead = {
  load: PostureLoad;
  posture: Posture;
};

const failureOf = ({
  failureCount,
  failureReason,
}: {
  failureCount: number;
  failureReason: unknown;
}): unknown => (failureCount > 0 ? failureReason : undefined);

export const usePosture = (): PostureRead => {
  const clusterId = useContext(ClusterContext);
  const polled = polledWhen(useContext(LiveRefreshContext));
  const now = useNow();
  const facts = useGetClusterFacts(
    clusterId === undefined ? skipToken : { pathParams: { id: clusterId } },
    polled,
  );
  const nodes = useListCoreV1Node({}, polled);
  const namespaces = useListCoreV1Namespace({}, polled);
  const requests = useListCertificatesV1CertificateSigningRequest({}, polled);
  const hasNoFacts = facts.data === undefined && facts.failureCount > 0;
  const lease = useReadCoordinationV1NamespacedLease(
    hasNoFacts
      ? { pathParams: { namespace: operatorLeaseNamespace, name: operatorLeaseName } }
      : skipToken,
    polled,
  );

  return {
    load: toPostureLoad(nodes),
    posture: toPosture(
      {
        facts: facts.data,
        lease: lease.data,
        leaseFailure: failureOf(lease),
        requests: requests.data?.items,
        requestsFailure: failureOf(requests),
        namespaces: namespaces.data?.items,
        nodes: nodes.data?.items ?? [],
      },
      now,
    ),
  };
};
