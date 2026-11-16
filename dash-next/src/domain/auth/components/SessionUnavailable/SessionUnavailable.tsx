import { AlertType, ButtonVariant, LayoutVariant } from '@ctrliq/quantic-components';
import { Alert, Button, Layout, Page } from '@ctrliq/quantic-react';

import { sessionUnavailable } from '../../constants';
import type { Failure } from '../../types/session';

type Props = {
  failure: Failure;
  onRetried: () => void;
};

export const SessionUnavailable = ({ failure, onRetried }: Props) => (
  <Layout variant={LayoutVariant.Focused}>
    <Page slot="body" contained>
      <Alert type={AlertType.Error}>
        <span slot="title">Session check failed</span>
        {sessionUnavailable} {failure.message}
        <Button slot="action" variant={ButtonVariant.Outline} onClick={onRetried}>
          Try again
        </Button>
      </Alert>
    </Page>
  </Layout>
);
