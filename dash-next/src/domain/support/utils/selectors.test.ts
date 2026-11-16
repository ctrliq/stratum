import { describe, expect, it } from 'vitest';

import type { ArchiveFlow, DownloadRead, StatusRead } from '../types/archive';
import { abandonedKeysOf, flowFor, flowKeyOf, outcomeOf, retryDelayAfter } from './selectors';

const cluster = { id: 'lab', uid: 'u-1', name: 'Lab', source: 'agent' } as const;
const identity = { key: 'lab/u-1/agent', clusterId: 'lab', clusterName: 'Lab' };
const starting: ArchiveFlow = { ...identity, phase: 'starting' };
const collecting: ArchiveFlow = {
  ...identity,
  phase: 'collecting',
  collectionId: 'c-1',
  acceptedAt: 1_000,
};
const unread: StatusRead = { data: undefined, error: null, dataUpdatedAt: 0 };
const idle: DownloadRead = { data: undefined, error: null, isSuccess: false };
const readAs = (outcome: string, compressedBytes: number | null, at = 3_000): StatusRead => ({
  data: { outcome, compressedBytes, manifest: null },
  error: null,
  dataUpdatedAt: at,
});
const forbidden = { status: 403, payload: { error: 'insufficient-role', message: 'admins only' } };
const unauthorized = { status: 401, payload: { error: 'unauthenticated', message: 'sign in' } };

describe('flowKeyOf and flowFor', () => {
  it('tell a cluster by its id, identity and registration source', () => {
    expect([
      flowKeyOf(cluster),
      flowFor([starting], cluster),
      flowFor([starting], { ...cluster, uid: 'u-2' }),
      flowFor([starting], { ...cluster, source: 'kubeconfig' }),
    ]).toEqual(['lab/u-1/agent', starting, undefined, undefined]);
  });
});

describe('retryDelayAfter', () => {
  it('waits 2, 4, 8 and then 15 seconds', () => {
    expect([0, 1, 2, 3, 4].map(retryDelayAfter)).toEqual([2_000, 4_000, 8_000, 15_000, 15_000]);
  });
});

describe('outcomeOf', () => {
  it('has none while the start, the collection or the download is under way', () => {
    expect([
      outcomeOf(starting, unread, idle),
      outcomeOf(collecting, unread, idle),
      outcomeOf(collecting, readAs('pending', null), idle),
      outcomeOf(collecting, readAs('partial', 10), idle),
    ]).toEqual([undefined, undefined, undefined, undefined]);
  });

  it('reports a start the hub refused, and ends silently on an unauthorized one', () => {
    expect([
      outcomeOf({ ...identity, phase: 'refused', error: forbidden }, unread, idle),
      outcomeOf({ ...identity, phase: 'refused', error: unauthorized }, unread, idle),
    ]).toEqual([{ type: 'failed', notice: 'Error: admins only' }, { type: 'silent' }]);
  });

  it('reports a start that names no collection', () => {
    expect(outcomeOf({ ...collecting, collectionId: '' }, unread, idle)).toEqual({
      type: 'failed',
      notice: 'The hub started the collection but answered no collection id',
    });
  });

  it('reports a status read that failed for good', () => {
    expect([
      outcomeOf(collecting, { ...unread, error: forbidden }, idle),
      outcomeOf(collecting, { ...readAs('pending', null), error: unauthorized }, idle),
    ]).toEqual([{ type: 'failed', notice: 'Error: admins only' }, { type: 'silent' }]);
  });

  it('reports a collection that ended without an archive', () => {
    expect(outcomeOf(collecting, readAs('failed', null), idle)).toEqual({
      type: 'failed',
      notice: "Evidence archive for Lab ended in failure — the hub's outcome is failed",
    });
  });

  it('gives up a collection still pending two minutes after it was accepted', () => {
    expect(outcomeOf(collecting, readAs('pending', null, 121_000), idle)).toEqual({
      type: 'failed',
      notice: 'Evidence archive for Lab did not finish in time',
    });
  });

  it('saves a downloaded archive under the fallback name', () => {
    const archive = new Blob(['x']);
    expect(
      outcomeOf(collecting, readAs('complete', 1), { data: archive, error: null, isSuccess: true }),
    ).toEqual({
      type: 'saved',
      notice: 'Evidence archive for Lab saved — outcome complete',
      archive,
      filename: 'c-1-support.tar.gz',
    });
  });

  it('reports a failed download and saves nothing', () => {
    expect([
      outcomeOf(collecting, readAs('complete', 1), { ...idle, error: forbidden }),
      outcomeOf(collecting, readAs('complete', 1), { ...idle, error: unauthorized }),
      outcomeOf(collecting, readAs('complete', 1), { ...idle, isSuccess: true }),
    ]).toEqual([
      { type: 'failed', notice: 'Error: admins only' },
      { type: 'silent' },
      { type: 'failed', notice: 'Error: the hub answered no archive file' },
    ]);
  });
});

describe('abandonedKeysOf', () => {
  it('names the started flows that are gone, so their start request can be aborted', () => {
    expect(abandonedKeysOf(['lab/u-1/agent', 'edge/u-2/agent'], [starting])).toEqual([
      'edge/u-2/agent',
    ]);
  });

  it('abandons nothing while every started flow is still running', () => {
    expect(abandonedKeysOf(['lab/u-1/agent'], [collecting])).toEqual([]);
  });
});
