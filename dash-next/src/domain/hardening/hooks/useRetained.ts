import { useState } from 'react';

import { withRetained } from '../utils/reducers';

export const useRetained = <TValue>(
  value: TValue | undefined,
  scope: string | undefined,
): TValue | undefined => {
  const [held, setHeld] = useState({ scope, value });
  const next = withRetained(held, scope, value);
  if (next !== held) {
    setHeld(next);
  }
  return next.value;
};
