import { AlertType, ButtonSize, ButtonVariant } from '@ctrliq/quantic-components';
import { Alert, Button } from '@ctrliq/quantic-react';

import { reconnecting } from '../../constants';

export const ReconnectingBanner = () => (
  <Alert slot="toolbar" type={AlertType.Warning}>
    <span slot="title">Reconnecting</span>
    {reconnecting}
    <Button
      slot="action"
      variant={ButtonVariant.Outline}
      size={ButtonSize.Small}
      onClick={() => {
        window.location.reload();
      }}
    >
      Reload
    </Button>
  </Alert>
);
