import { describe, expect, it } from 'vitest';

import type { Event } from '~/api/k8s/k8sSchemas';

import type { EventRow } from '../types';
import { toEventGroups, toEventRow, toEventRows } from './transforms';

const now = Date.parse('2026-11-13T12:00:00Z');

const toTimestamp = (minutesAgo: number): string =>
  new Date(now - minutesAgo * 60_000).toISOString();

const backOff: Event = {
  metadata: { name: 'web-1.17f', namespace: 'shop' },
  type: 'Warning',
  reason: 'BackOff',
  message: 'Back-off restarting failed container',
  count: 14,
  lastTimestamp: toTimestamp(3),
  involvedObject: { kind: 'Pod', name: 'web-1', namespace: 'shop' },
};

const bare: Event = { metadata: {}, involvedObject: {} };

describe('toEventRow', () => {
  it('carries type, reason, object, namespace, message, count and age', () => {
    expect(toEventRow(backOff, now)).toEqual({
      type: 'Warning',
      reason: 'BackOff',
      object: 'pod/web-1',
      namespace: 'shop',
      message: 'Back-off restarting failed container',
      count: 14,
      age: '3m',
      lastSeenAt: now - 3 * 60_000,
    });
  });

  it.each<[keyof ReturnType<typeof toEventRow>, string | number]>([
    ['type', 'Normal'],
    ['count', 1],
    ['reason', ''],
    ['message', ''],
    ['namespace', '—'],
    ['object', '/'],
  ])('defaults a missing %s to %j', (column, value) => {
    expect(toEventRow(bare, now)[column]).toBe(value);
  });

  it('falls back to the namespace of the object the event is about', () => {
    expect(
      toEventRow({ metadata: {}, involvedObject: { kind: 'Pod', namespace: 'shop' } }, now)
        .namespace,
    ).toBe('shop');
  });
});

describe('toEventRows', () => {
  const events: Event[] = Array.from({ length: 180 }, (_value, index) => ({
    metadata: { namespace: 'shop' },
    reason: `reason-${(index * 7) % 180}`,
    lastTimestamp: toTimestamp((index * 7) % 180),
    involvedObject: {},
  }));

  it('keeps the 50 most recently seen, newest first', () => {
    expect(toEventRows(events, now).map((row) => row.reason)).toEqual(
      Array.from({ length: 50 }, (_value, index) => `reason-${index}`),
    );
  });

  it('orders by each event’s best available timestamp', () => {
    expect(
      toEventRows(
        [
          { ...bare, reason: 'created', metadata: { creationTimestamp: toTimestamp(9) } },
          { ...bare, reason: 'last', lastTimestamp: toTimestamp(1) },
          { ...bare, reason: 'event', eventTime: toTimestamp(5) },
        ],
        now,
      ).map((row) => row.reason),
    ).toEqual(['last', 'event', 'created']);
  });
});

describe('toEventGroups', () => {
  const row = (overrides: Partial<EventRow>): EventRow => ({
    type: 'Normal',
    reason: 'FailedScheduling',
    object: 'pod/a',
    namespace: 'shop',
    message: '',
    count: 1,
    age: '5m',
    lastSeenAt: now - 5 * 60_000,
    ...overrides,
  });

  it('sums occurrences and counts distinct objects', () => {
    const [group] = toEventGroups([
      row({ count: 2 }),
      row({ object: 'pod/b', count: 1 }),
      row({ namespace: 'test', count: 3 }),
    ]);
    expect(group).toMatchObject({ reason: 'FailedScheduling', objectCount: 3, count: 6 });
  });

  it('is a Warning if any of its events is one', () => {
    expect(toEventGroups([row({}), row({ type: 'Warning' })])[0]?.type).toBe('Warning');
  });

  it('shows and orders groups by their most recent event', () => {
    const groups = toEventGroups([
      row({ reason: 'Pulled', age: '10m', lastSeenAt: now - 10 * 60_000 }),
      row({ reason: 'BackOff', age: '9m', lastSeenAt: now - 9 * 60_000 }),
      row({ reason: 'BackOff', age: '1m', lastSeenAt: now - 60_000 }),
    ]);
    expect(groups.map(({ reason, age }) => [reason, age])).toEqual([
      ['BackOff', '1m'],
      ['Pulled', '10m'],
    ]);
  });
});
