import { describe, expect, it } from 'vitest';

import { fromStatsSummary } from './parsers';

const summary = {
  node: { nodeName: 'cp-1' },
  pods: [
    {
      podRef: { namespace: 'shop', name: 'web-1', uid: '1' },
      cpu: { usageNanoCores: 37_600_000 },
      memory: { workingSetBytes: 1024 },
    },
    { podRef: { namespace: 'shop', name: 'web-2' } },
  ],
};

const expected = [
  {
    podRef: { namespace: 'shop', name: 'web-1' },
    cpu: { usageNanoCores: 37_600_000 },
    memory: { workingSetBytes: 1024 },
  },
  { podRef: { namespace: 'shop', name: 'web-2' } },
];

describe('fromStatsSummary', () => {
  it('reads the pods of a kubelet summary', () => {
    expect(fromStatsSummary(summary)).toEqual(expected);
  });

  it('reads a summary that arrived as text', () => {
    expect(fromStatsSummary(JSON.stringify(summary))).toEqual(expected);
  });

  it.each([['not json'], [undefined], [{ pods: 'none' }], [{}]])(
    'reads %o as no pods',
    (payload) => {
      expect(fromStatsSummary(payload)).toEqual([]);
    },
  );
});
