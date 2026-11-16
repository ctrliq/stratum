import { describe, expect, it } from 'vitest';

import { afterClose, afterOpen, withLines } from './reducers';

describe('exec state', () => {
  it('is connected once the socket opens', () => {
    expect(afterOpen()).toBe('connected');
  });

  it('is closed after a connected session ends', () => {
    expect(afterClose('connected')).toBe('closed');
  });

  it('stays in error when the connection never opened', () => {
    expect([afterClose('connecting'), afterClose('error')]).toEqual(['error', 'error']);
  });
});

describe('withLines', () => {
  it('appends and keeps only the newest lines', () => {
    expect(withLines(['a', 'b'], ['c', 'd'], 3)).toEqual(['b', 'c', 'd']);
  });

  it('keeps the newest 5000 by default', () => {
    const lines = Array.from({ length: 5001 }, (_unused, index) => String(index));
    expect([withLines(lines, []).length, withLines(lines, [])[0]]).toEqual([5000, '1']);
  });
});
