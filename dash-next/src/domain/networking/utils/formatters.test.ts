import { describe, expect, it } from 'vitest';

import { toServicePortText, toSlicePortText } from './formatters';

describe('toServicePortText', () => {
  it.each([
    [{ port: 80, protocol: 'TCP' }, '80/TCP'],
    [{ port: 80 }, '80/TCP'],
    [{ port: 80, nodePort: 30080 }, '80:30080/TCP'],
    [{ port: 53, protocol: 'UDP' }, '53/UDP'],
  ])('writes %o as %s', (port, shown) => {
    expect(toServicePortText(port)).toBe(shown);
  });
});

describe('toSlicePortText', () => {
  it.each([
    [{ port: 8080, protocol: 'TCP' }, '8080/TCP'],
    [{ port: 8080 }, '8080/TCP'],
    [{}, '*/TCP'],
    [{ port: 53, protocol: 'UDP' }, '53/UDP'],
  ])('writes %o as %s', (port, shown) => {
    expect(toSlicePortText(port)).toBe(shown);
  });
});
