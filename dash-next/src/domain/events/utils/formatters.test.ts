import { describe, expect, it } from 'vitest';

import {
  toCountText,
  toEventsMessage,
  toObjectCount,
  toRecentWarningCount,
  toRepeatText,
  toWarningCount,
} from './formatters';

describe('event texts', () => {
  it('shows a multiplier only for repeated events', () => {
    expect(toCountText({ count: 4 })).toBe('×4');
    expect(toCountText({ count: 1 })).toBe('1');
  });

  it('always shows a multiplier for a group', () => {
    expect(toRepeatText(1)).toBe('×1');
    expect(toRepeatText(6)).toBe('×6');
  });

  it('counts the objects a group touches', () => {
    expect(toObjectCount({ objectCount: 3 })).toBe('3 objects');
    expect(toObjectCount({ objectCount: 1 })).toBe('1 object');
  });

  it('counts warnings', () => {
    expect(toWarningCount(7)).toBe('7 warnings');
    expect(toRecentWarningCount(4)).toBe('4 warnings in the last hour');
  });
});

describe('toEventsMessage', () => {
  it('says no event matches when the filters hide every loaded event', () => {
    expect(toEventsMessage('No events found.', 12)).toBe('No matching events.');
  });

  it('keeps the message of a cluster with no events, and of an unreadable list', () => {
    expect(toEventsMessage('No events found.', 0)).toBe('No events found.');
    expect(toEventsMessage('You are not permitted to list events.', 0)).toBe(
      'You are not permitted to list events.',
    );
  });

  it('says nothing while rows are shown or still loading', () => {
    expect(toEventsMessage(undefined, 12)).toBeUndefined();
  });
});
