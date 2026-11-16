import { describe, expect, it } from 'vitest';

import { toRefusalText } from './formatters';

describe('toRefusalText', () => {
  it("is the cluster's own answer, whatever its form", () => {
    expect([
      toRefusalText(403, '{"kind":"Status","message":"pods \\"web\\" is forbidden"}\n'),
      toRefusalText(400, 'previous terminated container "app" not found'),
    ]).toEqual([
      '{"kind":"Status","message":"pods \\"web\\" is forbidden"}',
      'previous terminated container "app" not found',
    ]);
  });

  it('names the status when the body is empty', () => {
    expect([toRefusalText(403, ''), toRefusalText(502, ' \n')]).toEqual(['HTTP 403', 'HTTP 502']);
  });
});
