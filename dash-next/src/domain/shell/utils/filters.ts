import type { PaletteEntry } from '../types/screen';

const toSearchText = ({ title, subtitle = '', group }: PaletteEntry): string =>
  `${title} ${subtitle} ${group}`.toLowerCase();

export const matchingQuery = (entries: PaletteEntry[], query: string): PaletteEntry[] => {
  const wanted = query.trim().toLowerCase();
  return wanted === '' ? entries : entries.filter((entry) => toSearchText(entry).includes(wanted));
};
