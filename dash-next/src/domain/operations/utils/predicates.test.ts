import { describe, expect, it } from 'vitest';

import { hasOperationId, isCancellable, isGone, isTerminal } from './predicates';

describe('isTerminal', () => {
  it.each([
    ['accepted', false],
    ['queued', false],
    ['running', false],
    ['reconciling', false],
    ['completed', true],
    ['canceled', true],
    ['expired', true],
    ['something-new', false],
    [undefined, false],
  ])('state %s is terminal: %s', (state, verdict) => {
    expect(isTerminal({ state })).toBe(verdict);
  });
});

describe('isGone', () => {
  it.each([
    [404, true],
    [410, true],
    [403, false],
    [429, false],
    [500, false],
    ['unknown', false],
  ] as const)('an answer with status %s marks the operation gone: %s', (status, verdict) => {
    expect(isGone({ status, payload: {} })).toBe(verdict);
  });
});

describe('hasOperationId', () => {
  it('needs a body that carries the id of the operation', () => {
    expect([{ operationID: 'op-1' }, {}, undefined, null].map(hasOperationId)).toEqual([
      true,
      false,
      false,
      false,
    ]);
  });
});

describe('isCancellable', () => {
  it.each(['accepted', 'queued', 'running', 'reconciling', 'something-new', undefined])(
    'offers cancel to an admin while the state is %s',
    (state) => {
      expect([isCancellable({ state }, true), isCancellable({ state }, false)]).toEqual([
        true,
        false,
      ]);
    },
  );

  it.each(['completed', 'canceled', 'expired'])('offers no cancel once %s', (state) => {
    expect(isCancellable({ state }, true)).toBe(false);
  });

  it('offers no cancel once a cancellation is recorded', () => {
    expect(
      isCancellable({ state: 'running', cancelRequestedAt: '2026-11-13T12:00:00Z' }, true),
    ).toBe(false);
  });
});
