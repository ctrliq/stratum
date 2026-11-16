import { describe, expect, it } from 'vitest';

import { fromManifestText, fromReplicaText } from './parsers';

describe('fromManifestText', () => {
  it('reads a JSON object as a manifest', () => {
    expect(fromManifestText('{"kind":"ConfigMap","metadata":{"name":"settings"}}')).toEqual({
      isValid: true,
      manifest: { kind: 'ConfigMap', metadata: { name: 'settings' } },
    });
  });

  it("refuses text that is not JSON with the parser's message", () => {
    const result = fromManifestText('{"kind":');
    expect(result).toMatchObject({
      isValid: false,
      notice: expect.stringMatching(/^Invalid JSON: .+/),
    });
  });

  it.each(['null', '3', '[1, 2]'])('refuses the JSON %s, which is not an object', (text) => {
    expect(fromManifestText(text)).toEqual({
      isValid: false,
      notice: 'The manifest must be a JSON object.',
    });
  });
});

describe('fromReplicaText', () => {
  it.each([
    ['3', 3],
    ['0', 0],
    ['2147483647', 2147483647],
    ['', null],
    ['-1', null],
    ['1.5', null],
    ['1e3', null],
    ['0x10', null],
  ])('reads %j as %s', (text, count) => {
    expect(fromReplicaText(text)).toBe(count);
  });
});
