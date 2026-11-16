import { ButtonVariant } from '@ctrliq/quantic-components';
import {
  Alert,
  Button,
  Dialog,
  DialogActions,
  DialogTitle,
  Field,
  NumberInput,
  OptionCard,
  OptionCardGroup,
  Textarea,
} from '@ctrliq/quantic-react';
import { useForm, useStore } from '@tanstack/react-form';
import { useId } from 'react';

import { useCreateCluster, useListProviders } from '~/api/fleet/fleetComponents';
import { toast } from '~/shared/components/Toaster/toast';
import { formSubmitHandler } from '~/shared/utils/form';
import { toErrorMessage } from '~/shared/utils/formatters';

import { HintedField } from '../components/HintedField/HintedField';
import { HintedText } from '../components/HintedField/HintedText';
import {
  controlPlaneBounds,
  defaultKubernetesVersion,
  defaultNodeCount,
  deployNameHelp,
  noProviders,
  workerBounds,
} from '../constants';
import { useFleetRefresh } from '../hooks/useFleetRefresh';
import { deploymentSchema } from '../schemas/deployment';
import type { DeployValues, NamedCluster } from '../types';
import { toDeployRefusal, toNameHint, toPlacementHint, toVersionHint } from '../utils/formatters';
import { isDeployReady } from '../utils/predicates';
import { providerIdsIn, selectedProviderOf } from '../utils/selectors';
import { toDeployRequest, toNodeCount } from '../utils/transforms';
import styles from './DeployClusterDialog.module.scss';

type Props = {
  clusters: NamedCluster[];
  onClosed: () => void;
};

type CountProps = {
  label: string;
  hint: string;
  value: number;
  bounds: { min: number; max: number };
  onChanged: (value: number) => void;
};

const emptyDeployment: DeployValues = {
  provider: '',
  name: '',
  displayName: '',
  version: defaultKubernetesVersion,
  controlPlane: defaultNodeCount,
  workers: defaultNodeCount,
  placement: '',
};

const NodeCountField = ({ label, hint, value, bounds, onChanged }: CountProps) => {
  const fieldId = useId();
  return (
    <Field fieldId={fieldId} label={`${label} · ${value}`} hint={hint}>
      <NumberInput
        id={fieldId}
        value={value}
        min={bounds.min}
        max={bounds.max}
        step={1}
        onValueChanged={(event) => {
          if (event.detail.value !== null) {
            onChanged(toNodeCount(event.detail.value, bounds));
          }
        }}
      />
    </Field>
  );
};

export const DeployClusterDialog = ({ clusters, onClosed }: Props) => {
  const formId = useId();
  const onFleetChanged = useFleetRefresh();
  const providers = useListProviders(
    {},
    { select: providerIdsIn, staleTime: 0, gcTime: 0, retry: false },
  );
  const deploy = useCreateCluster({
    onSuccess: (_answer, { body }) => {
      toast(`Provisioning ${body.name}…`);
      void onFleetChanged();
      onClosed();
    },
  });
  const form = useForm({
    defaultValues: emptyDeployment,
    validators: { onSubmit: deploymentSchema },
    onSubmit: ({ value }) => {
      const chosen = {
        ...value,
        provider: selectedProviderOf(value.provider, providers.data ?? []),
      };
      if (isDeployReady(chosen, clusters) && !deploy.isPending) {
        deploy.mutate({ body: toDeployRequest(chosen) });
      }
    },
  });
  const values = useStore(form.store, (state) => state.values);
  const provider = selectedProviderOf(values.provider, providers.data ?? []);
  return (
    <Dialog
      open
      onOpenChanged={(event) => {
        if (!event.detail.open) {
          onClosed();
        }
      }}
    >
      <DialogTitle slot="header">
        Deploy a new cluster
        <span slot="subtitle">Provision infrastructure with ClusterAPI</span>
      </DialogTitle>
      <form id={formId} className={styles.main} onSubmit={formSubmitHandler(form)}>
        <section className={styles.providers}>
          <h3 className={styles.caption}>Provider</h3>
          {providers.data === undefined ? (
            providers.isError ? (
              <Alert type="error">
                <span slot="title">Providers unavailable</span>
                Could not list providers: {toErrorMessage(providers.error)}
              </Alert>
            ) : (
              <p role="status" className={styles.notice}>
                Loading…
              </p>
            )
          ) : providers.data.length === 0 ? (
            <p className={styles.notice}>{noProviders}</p>
          ) : (
            <OptionCardGroup
              aria-label="Infrastructure provider"
              value={provider}
              onValueChanged={(event) => {
                form.setFieldValue('provider', String(event.detail.value));
              }}
            >
              {providers.data.map((id) => (
                <OptionCard key={id} value={id} checked={id === provider}>
                  {id}
                </OptionCard>
              ))}
            </OptionCardGroup>
          )}
        </section>
        <form.Field name="name">
          {(field) => (
            <HintedText
              label="Cluster name"
              value={field.state.value}
              placeholder="staging-eu"
              hint={toNameHint(field.state.value, clusters, deployNameHelp)}
              onChanged={(name) => {
                field.handleChange(name.trim());
              }}
            />
          )}
        </form.Field>
        <form.Field name="displayName">
          {(field) => (
            <HintedText
              label="Display name"
              value={field.state.value}
              placeholder="Staging · EU"
              hint={{ text: 'optional', isError: false }}
              onChanged={field.handleChange}
            />
          )}
        </form.Field>
        <form.Field name="version">
          {(field) => (
            <HintedText
              label="Kubernetes version"
              value={field.state.value}
              placeholder={defaultKubernetesVersion}
              hint={toVersionHint(field.state.value)}
              onChanged={field.handleChange}
            />
          )}
        </form.Field>
        <form.Field name="controlPlane">
          {(field) => (
            <NodeCountField
              label="Control plane"
              hint="1 – 7; three or more survives losing one"
              value={field.state.value}
              bounds={controlPlaneBounds}
              onChanged={field.handleChange}
            />
          )}
        </form.Field>
        <form.Field name="workers">
          {(field) => (
            <NodeCountField
              label="Worker nodes"
              hint="0 – 100"
              value={field.state.value}
              bounds={workerBounds}
              onChanged={field.handleChange}
            />
          )}
        </form.Field>
        <form.Field name="placement">
          {(field) => (
            <HintedField
              fieldId={`${formId}-placement`}
              label="Placement values"
              hint={toPlacementHint(field.state.value)}
            >
              <Textarea
                id={`${formId}-placement`}
                rows={7}
                value={field.state.value}
                placeholder={'allowedNodes=pve1\ntemplateID=9000\nbridge=vmbr0'}
                onValueChanged={(event) => {
                  field.handleChange(event.detail.value);
                }}
              />
            </HintedField>
          )}
        </form.Field>
        <p className={styles.notice}>
          The fleet manager creates real machines through ClusterAPI and will be able to destroy
          them. It joins the fleet on its own once ClusterAPI publishes its kubeconfig — no agent is
          installed.
        </p>
        {deploy.error ? (
          <Alert type="error">
            <span slot="title">Deploy failed</span>
            {toDeployRefusal(deploy.error)}
          </Alert>
        ) : null}
      </form>
      <DialogActions slot="footer">
        <Button variant={ButtonVariant.Ghost} onClick={onClosed}>
          Cancel
        </Button>
        <Button
          type="submit"
          form={formId}
          disabled={!isDeployReady({ ...values, provider }, clusters) || deploy.isPending}
        >
          {deploy.isPending ? 'Submitting…' : 'Deploy cluster'}
        </Button>
      </DialogActions>
    </Dialog>
  );
};
