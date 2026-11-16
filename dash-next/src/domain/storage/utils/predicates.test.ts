import { describe, expect, it } from 'vitest';

import { isDefaultStorageClass } from './predicates';

describe('isDefaultStorageClass', () => {
  it.each([
    [{ 'storageclass.kubernetes.io/is-default-class': 'true' }, true],
    [{ 'storageclass.kubernetes.io/is-default-class': 'false' }, false],
    [{ 'storageclass.kubernetes.io/is-default-class': 'yes' }, false],
    [{}, false],
  ])('reads annotations %o as default: %s', (annotations, expected) => {
    expect(isDefaultStorageClass({ metadata: { annotations } })).toBe(expected);
  });
});
