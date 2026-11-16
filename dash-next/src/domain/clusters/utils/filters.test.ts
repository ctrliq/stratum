import { describe, expect, it } from 'vitest';

import { toCluster } from '../testing/fixtures';
import { evidenceCapabilitiesIn, matchingSearch } from './filters';

const prod = toCluster();
const other = toCluster({
  id: 'edge',
  name: 'edge',
  displayName: 'edge',
  labels: {},
  provider: 'proxmox',
  region: 'lab',
  status: 'degraded',
});

describe('matchingSearch', () => {
  it.each(['PROD', 'duction', 'aws', 'us-east', 'healthy', 'env=live'])(
    'finds the cluster by %s',
    (query) => {
      expect(matchingSearch([prod, other], query)).toEqual([prod]);
    },
  );

  it('shows every cluster for a blank search', () => {
    expect(matchingSearch([prod, other], '   ')).toEqual([prod, other]);
  });
});

describe('evidenceCapabilitiesIn', () => {
  it('keeps the three evidence actions in the order of the record', () => {
    expect(
      evidenceCapabilitiesIn(
        ['snapshot.restore', 'packages.install', 'certificates.check', 'snapshot.save'].map(
          (action) => ({ action }),
        ),
      ).map(({ action }) => action),
    ).toEqual(['snapshot.restore', 'certificates.check', 'snapshot.save']);
  });
});
