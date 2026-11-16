import { useCallback, useEffect, useState } from 'react';

import { collapsedStorageKey } from '../constants';
import { fromCollapsed } from '../utils/parsers';
import { withToggled } from '../utils/reducers';

const fromStorage = (): Record<string, boolean> => {
  try {
    return fromCollapsed(localStorage.getItem(collapsedStorageKey));
  } catch {
    return {};
  }
};

const isStored = (collapsed: Record<string, boolean>): boolean => {
  try {
    localStorage.setItem(collapsedStorageKey, JSON.stringify(collapsed));
    return true;
  } catch {
    return false;
  }
};

export const useCollapsedSections = () => {
  const [collapsed, setCollapsed] = useState(fromStorage);
  const onToggled = useCallback((id: string, isCollapsedNow: boolean) => {
    setCollapsed((current) => withToggled(current, id, isCollapsedNow));
  }, []);

  useEffect(() => {
    isStored(collapsed);
  }, [collapsed]);

  return { collapsed, onToggled };
};
