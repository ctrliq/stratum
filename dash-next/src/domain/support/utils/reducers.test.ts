import { describe, expect, it } from 'vitest';

import type { ArchiveFlow } from '../types/archive';
import { toFlows } from './reducers';

const lab = { id: 'lab', uid: 'u-1', name: 'Lab', source: 'agent' } as const;
const edge = { id: 'edge', uid: 'u-2', name: 'Edge', source: 'kubeconfig' } as const;
const labFlow: ArchiveFlow = {
  key: 'lab/u-1/agent',
  clusterId: 'lab',
  clusterName: 'Lab',
  phase: 'starting',
};
const edgeFlow: ArchiveFlow = {
  key: 'edge/u-2/kubeconfig',
  clusterId: 'edge',
  clusterName: 'Edge',
  phase: 'starting',
};

describe('toFlows', () => {
  it('starts one flow per cluster, each on its own', () => {
    const pressedTwice = toFlows(toFlows([], { type: 'pressed', cluster: lab }), {
      type: 'pressed',
      cluster: lab,
    });
    expect([pressedTwice, toFlows(pressedTwice, { type: 'pressed', cluster: edge })]).toEqual([
      [labFlow],
      [labFlow, edgeFlow],
    ]);
  });

  it('keeps the name the cluster had when the control was pressed', () => {
    expect(
      toFlows([labFlow], { type: 'accepted', key: labFlow.key, collectionId: 'c-1', at: 5 }),
    ).toEqual([{ ...labFlow, phase: 'collecting', collectionId: 'c-1', acceptedAt: 5 }]);
  });

  it('records a refused start on the starting flow of that cluster only', () => {
    expect(
      toFlows([labFlow, edgeFlow], { type: 'refused', key: edgeFlow.key, error: 'boom' }),
    ).toEqual([labFlow, { ...edgeFlow, phase: 'refused', error: 'boom' }]);
  });

  it('ignores an answer for a cluster with no starting flow', () => {
    const collecting: ArchiveFlow = {
      ...labFlow,
      phase: 'collecting',
      collectionId: 'c-1',
      acceptedAt: 5,
    };
    expect(
      toFlows([collecting], { type: 'accepted', key: labFlow.key, collectionId: 'c-2', at: 9 }),
    ).toEqual([collecting]);
  });

  it('never hands the answer of an abandoned start to the flow of a re-registered cluster', () => {
    const reRegistered: ArchiveFlow = { ...labFlow, key: 'lab/u-9/agent' };
    expect(
      toFlows([reRegistered], { type: 'accepted', key: labFlow.key, collectionId: 'c-1', at: 5 }),
    ).toEqual([reRegistered]);
  });

  it('drops an ended flow and the flows of clusters no longer listed', () => {
    expect([
      toFlows([labFlow, edgeFlow], { type: 'ended', key: labFlow.key }),
      toFlows([labFlow, edgeFlow], { type: 'listed', keys: [labFlow.key] }),
    ]).toEqual([[edgeFlow], [labFlow]]);
  });
});
