import { useState } from 'react';

import { layoutStorageKey } from '../constants';
import type { CatalogLayout } from '../types/catalog';
import { fromStoredLayout } from '../utils/parsers';

const fromStorage = (): string | null => {
  try {
    return localStorage.getItem(layoutStorageKey);
  } catch {
    return null;
  }
};

const isStoredAs = (layout: CatalogLayout): boolean => {
  try {
    localStorage.setItem(layoutStorageKey, layout);
    return true;
  } catch {
    return false;
  }
};

export const useCatalogLayout = () => {
  const [layout, setLayout] = useState<CatalogLayout>(() => fromStoredLayout(fromStorage()));
  const onLayoutChosen = (chosen: CatalogLayout): void => {
    setLayout(chosen);
    isStoredAs(chosen);
  };
  return { layout, onLayoutChosen };
};
