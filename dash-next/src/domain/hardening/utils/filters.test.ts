import { describe, expect, it } from 'vitest';

import { kubeletServingRequestsIn } from './filters';

describe('kubeletServingRequestsIn', () => {
  it('ignores requests for other signers', () => {
    const serving = { spec: { signerName: 'kubernetes.io/kubelet-serving' } };
    const client = { spec: { signerName: 'kubernetes.io/kube-apiserver-client' } };
    expect(kubeletServingRequestsIn([client, serving])).toEqual([serving]);
  });
});
