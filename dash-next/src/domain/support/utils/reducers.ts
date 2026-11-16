import type { ArchiveFlow, FlowAction } from '../types/archive';
import { flowKeyOf } from './selectors';

const isStarting = (flow: ArchiveFlow, key: string): boolean =>
  flow.phase === 'starting' && flow.key === key;

const identityOf = ({ key, clusterId, clusterName }: ArchiveFlow) => ({
  key,
  clusterId,
  clusterName,
});

export const toFlows = (flows: ArchiveFlow[], action: FlowAction): ArchiveFlow[] =>
  action.type === 'pressed'
    ? flows.some(({ key }) => key === flowKeyOf(action.cluster))
      ? flows
      : [
          ...flows,
          {
            key: flowKeyOf(action.cluster),
            clusterId: action.cluster.id,
            clusterName: action.cluster.name,
            phase: 'starting',
          },
        ]
    : action.type === 'accepted'
      ? flows.map((flow) =>
          isStarting(flow, action.key)
            ? {
                ...identityOf(flow),
                phase: 'collecting',
                collectionId: action.collectionId,
                acceptedAt: action.at,
              }
            : flow,
        )
      : action.type === 'refused'
        ? flows.map((flow) =>
            isStarting(flow, action.key)
              ? { ...identityOf(flow), phase: 'refused', error: action.error }
              : flow,
          )
        : action.type === 'ended'
          ? flows.filter(({ key }) => key !== action.key)
          : flows.filter(({ key }) => action.keys.includes(key));
