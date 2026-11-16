import { describe, expect, it } from 'vitest';

import { toOperation, toTarget } from '../testing/fixtures';
import { withCancelRequest, withPage } from './reducers';

const requestedAt = '2026-11-13T12:05:00Z';

describe('withPage', () => {
  it('keeps the header of the first page and appends the targets in the order served', () => {
    const first = toOperation({ targets: [toTarget({ childOperationID: 'a' })], terminalCount: 2 });
    const second = toOperation({
      targets: [toTarget({ childOperationID: 'b' })],
      terminalCount: 9,
    });
    const merged = withPage(withPage(undefined, first), second);
    expect([merged.terminalCount, merged.targets.map((target) => target.childOperationID)]).toEqual(
      [2, ['a', 'b']],
    );
  });
});

describe('withCancelRequest', () => {
  it('records the request time and keeps the full target set', () => {
    const shown = toOperation({ targets: [toTarget(), toTarget({ childOperationID: 'child-2' })] });
    expect(
      withCancelRequest(shown, toOperation({ targets: [], cancelRequestedAt: requestedAt })),
    ).toEqual({ ...shown, cancelRequestedAt: requestedAt });
  });

  it('changes nothing without an operation id or a request time in the answer', () => {
    const shown = toOperation();
    expect([
      withCancelRequest(shown, { cancelRequestedAt: requestedAt }),
      withCancelRequest(shown, toOperation()),
      withCancelRequest(shown, undefined),
      withCancelRequest(undefined, toOperation({ cancelRequestedAt: requestedAt })),
    ]).toEqual([shown, shown, shown, undefined]);
  });
});
