import { ConfirmDialog } from '@ctrliq/quantic-react';

import type { Confirmation } from '~/domain/catalog/types/catalog';
import { toUninstallHeading } from '~/domain/catalog/utils/formatters';
import { uninstallTargetOf } from '~/domain/catalog/utils/selectors';
import { toDisplayText } from '~/shared/utils/formatters';

type Props = {
  confirmation: Confirmation | undefined;
  onConfirmed: (confirmation: Confirmation) => void;
  onDismissed: () => void;
};

export const UninstallDialog = ({ confirmation, onConfirmed, onDismissed }: Props) => {
  const target = confirmation === undefined ? undefined : uninstallTargetOf(confirmation.chart);
  return (
    <ConfirmDialog
      open={confirmation !== undefined}
      heading={confirmation === undefined ? '' : toUninstallHeading(confirmation.app)}
      confirmText="Uninstall"
      destructive
      onConfirm={() => {
        if (confirmation !== undefined) {
          onConfirmed(confirmation);
        }
      }}
      onCancel={onDismissed}
    >
      {target === undefined ? null : (
        <p>
          Deletes the HelmChart{' '}
          <code>
            {toDisplayText(target.namespace)}/{toDisplayText(target.name)}
          </code>
          . The operator then runs <code>helm uninstall</code>, removing the release from namespace{' '}
          <code>{toDisplayText(target.targetNamespace)}</code>.
        </p>
      )}
      {confirmation?.app.notes ? <p>{confirmation.app.notes}</p> : null}
    </ConfirmDialog>
  );
};
