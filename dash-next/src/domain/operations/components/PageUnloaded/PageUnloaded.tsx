import { ButtonVariant } from '@ctrliq/quantic-components';
import { Button } from '@ctrliq/quantic-react';

import { EmptyState } from '~/shared/components/EmptyState/EmptyState';

import { loadingPage, unloadedPage } from '../../constants';

export const PageLoading = () => (
  <EmptyState fill>
    <span role="status">{loadingPage}</span>
  </EmptyState>
);

export const PageUnloaded = () => (
  <EmptyState
    fill
    title={unloadedPage}
    actions={
      <Button
        variant={ButtonVariant.Secondary}
        onClick={() => {
          window.location.reload();
        }}
      >
        Try again
      </Button>
    }
  />
);
