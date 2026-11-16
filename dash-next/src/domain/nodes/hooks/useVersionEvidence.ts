import { skipToken, useQueries } from '@tanstack/react-query';
import { useContext } from 'react';

import {
  getClusterReportEvidenceQuery,
  useGetClusterEngineState,
} from '~/api/fleet/fleetComponents';
import { allFilters } from '~/domain/hardening/constants';
import { useFindingsWalk } from '~/domain/hardening/hooks/useFindingsWalk';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { polledWhen, pollIntervalMs } from '~/shared/query/polling';

import { versionSignal } from '../constants';
import type { VersionView } from '../types/versions';
import { fromRefusal } from '../utils/parsers';
import { firstRefusalOf, unheldEvidenceRefsOf } from '../utils/selectors';
import { toEvidenceByRef, toVersionView } from '../utils/transforms';

type VersionEvidence =
  | { state: 'absent' }
  | { state: 'refused'; reason: string | undefined }
  | { state: 'loaded'; view: Extract<VersionView, { isReachable: true }> };

const noRefs: string[] = [];
const readOnce = {
  staleTime: Infinity,
  gcTime: 0,
  refetchOnWindowFocus: false,
  refetchOnReconnect: false,
};
const versionFilters = { ...allFilters, signal: versionSignal };

export const useVersionEvidence = (isOffered: boolean): VersionEvidence => {
  const clusterId = useContext(ClusterContext);
  const isLive = useContext(LiveRefreshContext);
  const summary = useGetClusterEngineState(
    isOffered && clusterId !== undefined ? { pathParams: { id: clusterId } } : skipToken,
    polledWhen(isLive),
  );
  const reportGeneration = summary.data?.reportGeneration;
  const { walk, refusal } = useFindingsWalk(
    clusterId !== undefined && reportGeneration
      ? { clusterId, reportGeneration, filters: versionFilters }
      : undefined,
    isLive ? pollIntervalMs : false,
    clusterId,
  );
  const evidenceRefs = walk === undefined ? noRefs : unheldEvidenceRefsOf(walk);
  const evidence = useQueries({
    queries:
      clusterId === undefined || walk === undefined
        ? []
        : evidenceRefs.map((evidenceRef) => ({
            ...getClusterReportEvidenceQuery({
              pathParams: { id: clusterId, reportGeneration: walk.reportGeneration, evidenceRef },
            }),
            ...readOnce,
          })),
    combine: (reads) => ({
      byRef: toEvidenceByRef(
        evidenceRefs,
        reads.map(({ data }) => data),
      ),
      refusal: firstRefusalOf(reads.map(({ error }) => error)),
    }),
  });
  const view = toVersionView({
    summary: summary.data,
    detail: walk,
    evidenceByRef: evidence.byRef,
  });

  return summary.failureReason !== null
    ? { state: 'refused', reason: fromRefusal(summary.failureReason) }
    : refusal !== undefined
      ? { state: 'refused', reason: refusal.code }
      : evidence.refusal !== undefined
        ? { state: 'refused', reason: fromRefusal(evidence.refusal) }
        : view.isReachable && walk !== undefined
          ? { state: 'loaded', view }
          : { state: 'absent' };
};
