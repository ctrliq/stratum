import { describe, expect, it } from 'vitest';

import type { Pod } from '~/api/k8s/k8sSchemas';

import { podsOn } from './filters';

const podIn = (phase: string, nodeName = 'node-a'): Pod => ({
  spec: { nodeName, containers: [] },
  status: { phase },
});

describe('podsOn', () => {
  it('leaves finished pods out of the count of a node', () => {
    const pods = [
      podIn('Running'),
      podIn('Running'),
      podIn('Running'),
      podIn('Pending'),
      podIn('Succeeded'),
      podIn('Succeeded'),
      podIn('Failed'),
    ];
    expect(podsOn(pods, { metadata: { name: 'node-a' } })).toHaveLength(4);
  });

  it('leaves out pods assigned to another node', () => {
    expect(podsOn([podIn('Running', 'node-b')], { metadata: { name: 'node-a' } })).toEqual([]);
  });
});
