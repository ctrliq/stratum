import { allFilters } from '../../constants';
import { useEngineState } from '../../hooks/useEngineState';
import { FindingsStatus } from '../EngineStateDrilldown/FindingsStatus';

type Props = {
  clusterId: string;
};

export const FindingsSurface = ({ clusterId }: Props) => {
  const { summary, walk, findingsRefusal, onReread } = useEngineState(clusterId, allFilters);
  return summary === undefined ? null : (
    <FindingsStatus summary={summary} walk={walk} refusal={findingsRefusal} onReread={onReread} />
  );
};
