import { ButtonVariant, DialogSize } from '@ctrliq/quantic-components';
import {
  Button,
  Dialog,
  DialogActions,
  DialogTitle,
  Field,
  TextInput,
} from '@ctrliq/quantic-react';
import { useForm, useStore } from '@tanstack/react-form';
import { useId } from 'react';

import { useDestroyCluster } from '~/api/fleet/fleetComponents';
import type { Cluster } from '~/api/fleet/fleetSchemas';
import { toast } from '~/shared/components/Toaster/toast';
import { formSubmitHandler } from '~/shared/utils/form';

import { useFleetRefresh } from '../hooks/useFleetRefresh';
import { toActionError, toDestroyFacts } from '../utils/formatters';
import { isDestroyConfirmed } from '../utils/predicates';
import styles from './DestroyClusterDialog.module.scss';

type Props = {
  cluster: Pick<Cluster, 'id' | 'name' | 'provider' | 'nodeCount' | 'status'>;
  onClosed: () => void;
};

export const DestroyClusterDialog = ({ cluster, onClosed }: Props) => {
  const formId = useId();
  const onFleetChanged = useFleetRefresh();
  const destroy = useDestroyCluster({
    onSuccess: () => {
      toast(`Destroying ${cluster.name}…`);
      void onFleetChanged();
    },
    onError: (error) => {
      toast.error(toActionError(error));
    },
    onSettled: onClosed,
  });
  const form = useForm({
    defaultValues: { confirmation: '' },
    onSubmit: ({ value }) => {
      if (isDestroyConfirmed(value.confirmation, cluster) && !destroy.isPending) {
        destroy.mutate({
          pathParams: { id: cluster.id },
          body: { confirm: value.confirmation.trim() },
        });
      }
    },
  });
  const confirmation = useStore(form.store, (state) => state.values.confirmation);
  return (
    <Dialog
      open
      size={DialogSize.Small}
      onOpenChanged={(event) => {
        if (!event.detail.open) {
          onClosed();
        }
      }}
    >
      <DialogTitle slot="header">Destroy {cluster.name}?</DialogTitle>
      <form id={formId} className={styles.main} onSubmit={formSubmitHandler(form)}>
        <p>
          This deletes <strong>{cluster.name}</strong> and every machine ClusterAPI built for it.
          There is no undo, and the cluster&apos;s workloads go with it.
        </p>
        <p className={styles.facts}>{toDestroyFacts(cluster)}</p>
        <p className={styles.facts}>
          The record stays visible in <code>Deleting</code> until its machines are actually gone. To
          remove the record without touching the infrastructure, use Unregister instead.
        </p>
        <form.Field name="confirmation">
          {(field) => (
            <Field fieldId={`${formId}-confirmation`} label={`Type ${cluster.id} to confirm`}>
              <TextInput
                id={`${formId}-confirmation`}
                name={field.name}
                value={field.state.value}
                placeholder={cluster.id}
                onValueChanged={(event) => {
                  field.handleChange(event.detail.value);
                }}
              />
            </Field>
          )}
        </form.Field>
      </form>
      <DialogActions slot="footer">
        <Button variant={ButtonVariant.Ghost} onClick={onClosed}>
          Cancel
        </Button>
        <Button
          destructive
          type="submit"
          form={formId}
          disabled={!isDestroyConfirmed(confirmation, cluster) || destroy.isPending}
        >
          {destroy.isPending ? 'Destroying…' : 'Destroy this cluster'}
        </Button>
      </DialogActions>
    </Dialog>
  );
};
