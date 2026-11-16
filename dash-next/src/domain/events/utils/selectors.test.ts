import { describe, expect, it } from 'vitest';

import { involvedObjectOf, lastSeenOf } from './selectors';

const lastTimestamp = '2026-11-13T11:57:00Z';
const eventTime = '2026-11-13T11:50:00.000000Z';
const creationTimestamp = '2026-11-13T11:40:00Z';

describe('lastSeenOf', () => {
  it.each([
    [{ lastTimestamp, eventTime, metadata: { creationTimestamp } }, lastTimestamp],
    [{ eventTime, metadata: { creationTimestamp } }, eventTime],
    [{ metadata: { creationTimestamp } }, creationTimestamp],
  ])('picks the best available timestamp of %o', (event, chosen) => {
    expect(lastSeenOf(event)).toBe(chosen);
  });
});

describe('involvedObjectOf', () => {
  it('reads kind and name, the kind in lower case', () => {
    expect(involvedObjectOf({ involvedObject: { kind: 'Pod', name: 'web-1' } })).toBe('pod/web-1');
  });

  it('is a bare slash without an involved object', () => {
    expect(involvedObjectOf({ involvedObject: {} })).toBe('/');
  });
});
