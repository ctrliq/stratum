import type { PaletteState } from '../types/screen';

export const freshPalette: PaletteState = { query: '', highlighted: 0 };

export const withQuery = (state: PaletteState, query: string): PaletteState => ({
  ...state,
  query,
  highlighted: 0,
});

export const withHighlight = (state: PaletteState, highlighted: number): PaletteState => ({
  ...state,
  highlighted,
});

export const withHighlightMoved = (
  state: PaletteState,
  step: number,
  entryCount: number,
): PaletteState => ({
  ...state,
  highlighted: Math.max(0, Math.min(entryCount - 1, state.highlighted + step)),
});

export const withToggled = (
  collapsed: Record<string, boolean>,
  id: string,
  isCollapsedNow: boolean,
): Record<string, boolean> => ({ ...collapsed, [id]: !isCollapsedNow });
