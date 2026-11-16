import { ConfirmDialog } from '@ctrliq/quantic-react';

import { useUnregisterCluster } from '~/api/fleet/fleetComponents';
import type { Cluster } from '~/api/fleet/fleetSchemas';
import { toast } from '~/shared/components/Toaster/toast';

import { useFleetRefresh } from '../hooks/useFleetRefresh';
import { toUnregisterError } from '../utils/formatters';
import { cleanupCommandIn } from '../utils/selectors';

type Props = {
  cluster: Pick<Cluster, 'id' | 'name'>;
  onClosed: () => void;
  onCleanupOffered: (command: string) => void;
};

export const UnregisterClusterDialog = ({ cluster, onClosed, onCleanupOffered }: Props) => {
  const onFleetChanged = useFleetRefresh();
  const unregister = useUnregisterCluster({
    onSuccess: (answer) => {
      const command = cleanupCommandIn(answer);
      toast(`${cluster.name} removed from the fleet manager`);
      void onFleetChanged();
      if (command === '') {
        onClosed();
      } else {
        onCleanupOffered(command);
      }
    },
    onError: (error) => {
      toast.error(toUnregisterError(error));
      onClosed();
    },
  });
  return (
    <ConfirmDialog
      open
      destructive
      heading={`Unregister ${cluster.name}?`}
      message={`Removes ${cluster.name} from the fleet manager. The cluster keeps running — it just won't be visible or manageable from here. You can re-register it later.`}
      confirmText="Unregister"
      cancelText="Cancel"
      loading={unregister.isPending}
      onConfirm={(event) => {
        event.preventDefault();
        unregister.mutate({ pathParams: { id: cluster.id } });
      }}
      onCancel={onClosed}
    />
  );
};
