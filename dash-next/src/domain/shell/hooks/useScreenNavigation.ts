import { useRouter } from '@tanstack/react-router';
import { useCallback } from 'react';

import { isPlainClick } from '~/shared/utils/predicates';

type LinkClick = Parameters<typeof isPlainClick>[0] & {
  preventDefault: () => void;
};

export const useScreenNavigation = () => {
  const router = useRouter();
  const onOpened = useCallback(
    (href: string) => {
      router.history.push(href);
    },
    [router],
  );
  const onLinkClicked = useCallback(
    (event: LinkClick, href: string) => {
      if (isPlainClick(event)) {
        event.preventDefault();
        router.history.push(href);
      }
    },
    [router],
  );
  return { onOpened, onLinkClicked };
};
