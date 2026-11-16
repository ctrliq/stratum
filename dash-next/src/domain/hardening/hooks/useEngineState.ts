import { useContext } from 'react';

import { useGetClusterEngineState } from '~/api/fleet/fleetComponents';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { polledWhen, pollIntervalMs } from '~/shared/query/polling';
import { isRecord } from '~/shared/utils/predicates';

import { notAnObjectMessage } from '../constants';
import type { FindingFilters, FindingsWalk, Refusal, SummaryView } from '../types/engineState';
import { detailGenerationOf } from '../utils/selectors';
import { toRefusal } from '../utils/transforms';
import { useFindingsWalk } from './useFindingsWalk';

type EngineState = {
  summary: SummaryView | undefined;
  summaryRefusal: Refusal | undefined;
  walk: FindingsWalk | undefined;
  findingsRefusal: Refusal | undefined;
  onSummaryReread: () => void;
  onReread: () => void;
};

const notAnObject = toRefusal({ status: 200, payload: { message: notAnObjectMessage } });

export const useEngineState = (clusterId: string, filters: FindingFilters): EngineState => {
  const isLive = useContext(LiveRefreshContext);
  const { data, failureCount, failureReason, refetch } = useGetClusterEngineState(
    { pathParams: { id: clusterId } },
    polledWhen(isLive),
  );
  const summary: SummaryView | undefined = isRecord(data) ? data : undefined;
  const reportGeneration = detailGenerationOf(summary);
  const { walk, refusal, onRewalked } = useFindingsWalk(
    reportGeneration === undefined ? undefined : { clusterId, reportGeneration, filters },
    isLive ? pollIntervalMs : false,
    clusterId,
  );

  return {
    summary,
    summaryRefusal:
      failureCount > 0
        ? toRefusal(failureReason)
        : data !== undefined && summary === undefined
          ? notAnObject
          : undefined,
    walk,
    findingsRefusal: refusal,
    onSummaryReread: () => void refetch(),
    onReread: () => {
      void refetch();
      onRewalked();
    },
  };
};
