import { useMutation } from '@tanstack/react-query';
import { type ReactNode, useCallback, useEffect, useMemo, useReducer, useRef } from 'react';

import { fetchCreateSupportArchive } from '~/api/fleet/fleetComponents';
import { useCanWrite } from '~/domain/auth/hooks/useCanWrite';

import { startDeadlineMs } from '../../constants';
import { ArchiveFlowsContext } from '../../hooks/useArchiveFlows';
import type { ArchiveCluster } from '../../types/archive';
import { fromReceipt } from '../../utils/parsers';
import { toFlows } from '../../utils/reducers';
import { abandonedKeysOf, flowKeyOf } from '../../utils/selectors';
import { ArchiveFlowRunner } from './ArchiveFlowRunner';

type Props = {
  clusters: ArchiveCluster[];
  children: ReactNode;
};

type Start = {
  cluster: ArchiveCluster;
  signal: AbortSignal;
};

type Starts = Map<string, AbortController>;

const abortedAmong = (starts: Starts, keys: string[]): Starts => {
  for (const key of keys) {
    starts.get(key)?.abort();
  }
  return new Map([...starts].filter(([key]) => !keys.includes(key)));
};

const FlowsOfIdentity = ({ clusters, children }: Props) => {
  const [flows, dispatch] = useReducer(toFlows, []);
  const starts = useRef<Starts>(new Map());
  const { mutate: started } = useMutation({
    mutationFn: ({ cluster, signal }: Start) =>
      fetchCreateSupportArchive(
        { pathParams: { id: cluster.id }, deadlineMs: startDeadlineMs },
        signal,
      ),
    onSuccess: (receipt, { cluster }) => {
      dispatch({
        type: 'accepted',
        key: flowKeyOf(cluster),
        collectionId: fromReceipt(receipt),
        at: Date.now(),
      });
    },
    onError: (error, { cluster, signal }) => {
      if (!signal.aborted) {
        dispatch({ type: 'refused', key: flowKeyOf(cluster), error });
      }
    },
  });
  const keys = clusters.map(flowKeyOf);
  if (flows.some(({ key }) => !keys.includes(key))) {
    dispatch({ type: 'listed', keys });
  }
  useEffect(() => {
    starts.current = abortedAmong(
      starts.current,
      abandonedKeysOf([...starts.current.keys()], flows),
    );
  }, [flows]);
  useEffect(
    () => () => {
      starts.current = abortedAmong(starts.current, [...starts.current.keys()]);
    },
    [],
  );
  const onPressed = useCallback(
    (cluster: ArchiveCluster): void => {
      const start = new AbortController();
      starts.current = new Map([...starts.current, [flowKeyOf(cluster), start]]);
      dispatch({ type: 'pressed', cluster });
      started({ cluster, signal: start.signal });
    },
    [started],
  );
  const onEnded = useCallback((key: string): void => {
    dispatch({ type: 'ended', key });
  }, []);
  const value = useMemo(() => ({ flows, onPressed }), [flows, onPressed]);

  return (
    <ArchiveFlowsContext.Provider value={value}>
      {flows.map((flow) => (
        <ArchiveFlowRunner key={flow.key} flow={flow} onEnded={onEnded} />
      ))}
      {children}
    </ArchiveFlowsContext.Provider>
  );
};

export const EvidenceArchiveFlows = (props: Props) => (
  <FlowsOfIdentity key={String(useCanWrite())} {...props} />
);
