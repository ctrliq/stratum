import { describe, expect, it } from 'vitest';

import { unresolvedConfig } from '~/shared/context/ConfigContext';

import {
  isArchiveFile,
  isArchiveOffered,
  isArchiveReady,
  isAwaited,
  isCollectable,
  isOverdue,
  isReadRetried,
  isStillCollecting,
  isUnauthorized,
} from './predicates';

const hub = { ...unresolvedConfig, auth: { required: true, providers: ['local'] } };
const dashboardServer = { ...unresolvedConfig, auth: { required: true, providers: ['token'] } };
const pending = { outcome: 'pending', compressedBytes: null };
const finished = { outcome: 'partial', compressedBytes: 2048 };

describe('isArchiveOffered', () => {
  it('is true only for a writer on the hub', () => {
    expect([
      isArchiveOffered(hub, true),
      isArchiveOffered(hub, false),
      isArchiveOffered(dashboardServer, true),
    ]).toEqual([true, false, false]);
  });
});

describe('isCollectable', () => {
  it.each([
    ['healthy', true],
    ['degraded', true],
    ['disconnected', false],
    ['pending', false],
    ['provisioning', false],
    ['deleting', false],
    [undefined, false],
  ] as const)('a cluster whose phase is %s: %s', (status, expected) => {
    expect(isCollectable({ status })).toBe(expected);
  });
});

describe('isArchiveReady and isStillCollecting', () => {
  it('go by the archive size first and the outcome second', () => {
    expect(
      [
        undefined,
        pending,
        { outcome: '', compressedBytes: null },
        { outcome: 'staging', compressedBytes: null },
        { outcome: 'failed', compressedBytes: null },
        finished,
        { outcome: 'pending', compressedBytes: 0 },
      ].map((collection) => [isArchiveReady(collection), isStillCollecting(collection)]),
    ).toEqual([
      [false, true],
      [false, true],
      [false, true],
      [false, true],
      [false, false],
      [true, false],
      [true, false],
    ]);
  });
});

describe('isOverdue and isAwaited', () => {
  it('give a pending collection two minutes from its acceptance', () => {
    expect([isOverdue(119_999, 0), isOverdue(120_000, 0), isOverdue(130_000, 20_000)]).toEqual([
      false,
      true,
      false,
    ]);
  });

  it('keep reading only a pending collection that is not overdue', () => {
    expect([
      isAwaited({ data: undefined, dataUpdatedAt: 0 }, 0),
      isAwaited({ data: { ...pending, manifest: null }, dataUpdatedAt: 5_000 }, 0),
      isAwaited({ data: { ...pending, manifest: null }, dataUpdatedAt: 120_000 }, 0),
      isAwaited({ data: { ...finished, manifest: null }, dataUpdatedAt: 5_000 }, 0),
    ]).toEqual([true, true, false, false]);
  });
});

describe('isReadRetried', () => {
  it('retries dropped reads and server errors four times, and never a refusal', () => {
    const dropped = { status: 'unknown', payload: 'Failed to fetch' };
    expect([
      isReadRetried(0, dropped),
      isReadRetried(3, { status: 503, payload: {} }),
      isReadRetried(4, dropped),
      isReadRetried(0, { status: 403, payload: {} }),
      isReadRetried(0, { status: 404, payload: {} }),
      isReadRetried(0, { status: 401, payload: {} }),
    ]).toEqual([true, true, false, false, false, false]);
  });
});

describe('isUnauthorized and isArchiveFile', () => {
  it('recognise a 401 answer and a binary body', () => {
    expect([
      isUnauthorized({ status: 401, payload: {} }),
      isUnauthorized({ status: 403, payload: {} }),
      isArchiveFile(new Blob(['x'])),
      isArchiveFile({}),
    ]).toEqual([true, false, true, false]);
  });
});
