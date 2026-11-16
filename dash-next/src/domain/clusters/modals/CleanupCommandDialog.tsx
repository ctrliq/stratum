import { Button, Dialog, DialogActions, DialogTitle } from '@ctrliq/quantic-react';

import { CopyableCommand } from '../components/CopyableCommand/CopyableCommand';

type Props = {
  clusterName: string;
  command: string;
  onClosed: () => void;
};

export const CleanupCommandDialog = ({ clusterName, command, onClosed }: Props) => (
  <Dialog
    open
    onOpenChanged={(event) => {
      if (!event.detail.open) {
        onClosed();
      }
    }}
  >
    <DialogTitle slot="header">{clusterName} unregistered</DialogTitle>
    <p>
      The agent credential has been revoked. To remove the fleet agent from the cluster itself, run
      this against it:
    </p>
    <CopyableCommand label="Cleanup command" text={command} />
    <DialogActions slot="footer">
      <Button onClick={onClosed}>Done</Button>
    </DialogActions>
  </Dialog>
);
