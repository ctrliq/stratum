import { describe, expect, it } from 'vitest';

import { isSecretTable } from './predicates';

describe('isSecretTable', () => {
  it('accepts a Table answer and refuses a plain list or anything else', () => {
    expect(
      [{ columnDefinitions: [], rows: [] }, { items: [] }, { rows: [] }, null, 'Table'].map(
        isSecretTable,
      ),
    ).toEqual([true, false, false, false, false]);
  });
});
