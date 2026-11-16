import { describe, expect, it } from 'vitest';

import {
  toArchiveFilename,
  toControlCaption,
  toFailedNotice,
  toOverdueNotice,
  toSavedNotice,
} from './formatters';

const section = (name: string, state: string) => ({ name, state });

describe('toControlCaption', () => {
  it.each([
    ['healthy', 'Download evidence archive'],
    ['degraded', 'Download evidence archive'],
    ['disconnected', 'Cannot collect an evidence archive — the cluster is disconnected'],
    ['pending', 'Cannot collect an evidence archive — the cluster is pending'],
    ['provisioning', 'Cannot collect an evidence archive — the cluster is provisioning'],
    ['deleting', 'Cannot collect an evidence archive — the cluster is deleting'],
    [undefined, 'Cannot collect an evidence archive — the cluster is in an unknown phase'],
  ] as const)('for phase %s reads %s', (status, caption) => {
    expect(toControlCaption({ status })).toBe(caption);
  });
});

describe('toFailedNotice and toOverdueNotice', () => {
  it('name the cluster', () => {
    expect([toFailedNotice('lab', 'failed'), toOverdueNotice('lab')]).toEqual([
      "Evidence archive for lab ended in failure — the hub's outcome is failed",
      'Evidence archive for lab did not finish in time',
    ]);
  });
});

describe('toSavedNotice', () => {
  it('states the outcome', () => {
    expect(toSavedNotice('lab', { outcome: 'complete', manifest: null })).toBe(
      'Evidence archive for lab saved — outcome complete',
    );
  });

  it('names every section that is not complete', () => {
    expect(
      toSavedNotice('lab', {
        outcome: 'partial',
        manifest: {
          sections: [
            section('logs', 'truncated'),
            section('events', 'stale'),
            section('nodes', 'complete'),
          ],
        },
      }),
    ).toBe(
      'Evidence archive for lab saved — outcome partial; incomplete sections: logs truncated, events stale',
    );
  });
});

describe('toArchiveFilename', () => {
  it('takes the name the hub gave the file, else one made from the collection', () => {
    expect([
      toArchiveFilename('c-1', new File(['x'], 'lab-support.tar.gz')),
      toArchiveFilename('c-1', new Blob(['x'])),
    ]).toEqual(['lab-support.tar.gz', 'c-1-support.tar.gz']);
  });
});
