import { useEffect, useState } from 'react';

import { clustersViewStorageKey } from '../constants';
import type { ClustersView } from '../types';
import { fromStoredView } from '../utils/parsers';

const fromStorage = (): ClustersView => {
  try {
    return fromStoredView(localStorage.getItem(clustersViewStorageKey));
  } catch {
    return fromStoredView(null);
  }
};

const isStored = (view: ClustersView): boolean => {
  try {
    localStorage.setItem(clustersViewStorageKey, view);
    return true;
  } catch {
    return false;
  }
};

export const useClustersView = () => {
  const [view, setView] = useState(fromStorage);
  useEffect(() => {
    isStored(view);
  }, [view]);
  return { view, onViewChosen: setView };
};
