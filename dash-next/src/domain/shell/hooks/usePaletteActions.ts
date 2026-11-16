import { createContext, useContext, useEffect } from 'react';

import type { PaletteAction } from '../types/screen';

type Withdrawal = () => void;

export const PaletteActionsContext = createContext<(action: PaletteAction) => Withdrawal>(
  () => () => undefined,
);

export const usePaletteAction = (action: PaletteAction | undefined): void => {
  const onOffered = useContext(PaletteActionsContext);
  useEffect(() => (action ? onOffered(action) : undefined), [action, onOffered]);
};
