import { describe, expect, it } from 'vitest';

import {
  fromCpuQuantity,
  fromJsonText,
  fromMemoryQuantity,
  fromObjectReference,
  fromSearchString,
} from './parsers';

describe('fromCpuQuantity', () => {
  it.each([
    ['500m', 500],
    ['2', 2000],
    ['0.5', 500],
    ['250000000n', 250],
    ['1500u', 1.5],
    [2, 2000],
    [undefined, 0],
  ])('reads %s as %s millicores', (quantity, millicores) => {
    expect(fromCpuQuantity(quantity)).toBeCloseTo(millicores, 9);
  });

  it('reads an unparseable quantity as zero', () => {
    expect(fromCpuQuantity('abc')).toBe(0);
  });
});

describe('fromMemoryQuantity', () => {
  it.each([
    ['512Mi', 512],
    ['2Gi', 2048],
    ['1.5Gi', 1536],
    ['1048576Ki', 1024],
    ['1Ti', 1048576],
    ['1Pi', 1073741824],
    ['1048576', 1],
    [undefined, 0],
    ['abc', 0],
    ['-5Mi', 0],
    ['1Ei', 1099511627776],
    ['5Zi', 0],
    ['1e3Mi', 1000],
  ])('reads %s as exactly %s Mi', (quantity, mebibytes) => {
    expect(fromMemoryQuantity(quantity)).toBe(mebibytes);
  });

  it.each([
    ['1M', 0.954, 3],
    ['1G', 953.67, 2],
    ['1T', 953674.3, 1],
    ['1K', 0.00095, 5],
    ['100k', 0.0954, 4],
    ['1500m', 0.0000014, 7],
    ['1P', 953674316, 0],
    ['1E', 953674316406, 0],
    ['129e6', 123.02, 2],
  ])('reads %s as about %s Mi', (quantity, mebibytes, digits) => {
    expect(fromMemoryQuantity(quantity)).toBeCloseTo(mebibytes, digits);
  });
});

describe('fromObjectReference', () => {
  it('reads a namespace and a name', () => {
    expect(fromObjectReference('payments/api')).toEqual({ namespace: 'payments', name: 'api' });
  });

  it('reads a bare name without a namespace', () => {
    expect(fromObjectReference('api')).toEqual({ name: 'api' });
  });

  it('uses only the first two slash-separated parts', () => {
    expect(fromObjectReference('a/b/c')).toEqual({ namespace: 'a', name: 'b' });
  });
});

describe('fromJsonText', () => {
  it('returns the parsed value', () => {
    expect(fromJsonText('{"kind":"ConfigMap"}')).toEqual({
      isValid: true,
      value: { kind: 'ConfigMap' },
    });
  });

  it("returns the parser's message for invalid text", () => {
    const result = fromJsonText('{nope');
    expect(result.isValid).toBe(false);
    expect(result).toHaveProperty('message', expect.stringMatching(/JSON/));
  });
});

describe('fromSearchString', () => {
  it('keeps every value as the text the address carries', () => {
    expect(fromSearchString('?name=123&tab=true&ns=null&q=a%20b')).toEqual({
      name: '123',
      tab: 'true',
      ns: 'null',
      q: 'a b',
    });
  });

  it('keeps a repeated parameter as a list and reads nothing from an empty query', () => {
    expect([fromSearchString('op=a&op=b'), fromSearchString('')]).toEqual([{ op: ['a', 'b'] }, {}]);
  });
});
