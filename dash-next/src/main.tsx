import './index.scss';

import { inter, satoshi } from '@ctrliq/quantic-fonts';
import { QueryClientProvider } from '@tanstack/react-query';
import { createRouter, RouterProvider } from '@tanstack/react-router';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { queryClient } from '~/shared/query/client';
import { toSearchString } from '~/shared/utils/formatters';
import { fromSearchString } from '~/shared/utils/parsers';

import { routeTree } from './routeTree.gen';

inter.inject();
satoshi.inject();

const router = createRouter({
  routeTree,
  parseSearch: fromSearchString,
  stringifySearch: toSearchString,
});

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router;
  }
}

const root = document.getElementById('root');

if (root) {
  createRoot(root).render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </StrictMode>,
  );
}
