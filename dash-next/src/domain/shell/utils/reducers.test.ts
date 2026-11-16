import { describe, expect, it } from 'vitest';

import {
  freshPalette,
  withHighlight,
  withHighlightMoved,
  withQuery,
  withToggled,
} from './reducers';

describe('withQuery', () => {
  it('moves the highlight back to the first entry', () => {
    expect(withQuery({ query: 'po', highlighted: 3 }, 'pod')).toEqual({
      query: 'pod',
      highlighted: 0,
    });
  });
});

describe('withHighlight', () => {
  it('highlights the pointed entry', () => {
    expect(withHighlight(freshPalette, 4).highlighted).toBe(4);
  });
});

describe('withHighlightMoved', () => {
  it('moves one entry and stops at both ends', () => {
    expect(
      [
        withHighlightMoved({ query: '', highlighted: 1 }, 1, 3),
        withHighlightMoved({ query: '', highlighted: 2 }, 1, 3),
        withHighlightMoved({ query: '', highlighted: 0 }, -1, 3),
        withHighlightMoved({ query: '', highlighted: 0 }, 1, 0),
      ].map(({ highlighted }) => highlighted),
    ).toEqual([2, 2, 0, 0]);
  });
});

describe('withToggled', () => {
  it('flips one section and keeps the others', () => {
    expect(withToggled({ config: true }, 'workloads', false)).toEqual({
      config: true,
      workloads: true,
    });
    expect(withToggled({ workloads: true }, 'workloads', true)).toEqual({ workloads: false });
  });
});
