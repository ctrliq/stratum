import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef } from 'react';

import { toReadWarning } from '../utils/formatters';
import { isFactsRead, isReadFailure } from '../utils/predicates';
import { pathOf } from '../utils/selectors';

export const useReadWarnings = (): void => {
  const queryClient = useQueryClient();
  const reportedFacts = useRef(new Set<string>());

  useEffect(
    () =>
      queryClient.getQueryCache().subscribe((event) => {
        if (event.type === 'updated' && isReadFailure(event.action)) {
          const { queryKey } = event.query;
          const isRepeatedFacts =
            isFactsRead(queryKey) && reportedFacts.current.has(pathOf(queryKey));
          if (!isRepeatedFacts) {
            reportedFacts.current.add(pathOf(queryKey));
            console.warn(toReadWarning(queryKey, event.action.error));
          }
        }
      }),
    [queryClient],
  );
};
