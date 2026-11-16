import { describe, expect, it } from 'vitest';

import { matchingFilter } from './filters';

const lines = ['GET /health 200', 'ERROR timeout'];

describe('matchingFilter', () => {
  it('keeps lines containing the text, ignoring case', () => {
    expect(matchingFilter(lines, 'error')).toEqual(['ERROR timeout']);
  });

  it('keeps every line without a filter', () => {
    expect(matchingFilter(lines, '')).toBe(lines);
  });
});
