import { ButtonVariant } from '@ctrliq/quantic-components';
import {
  Alert,
  Button,
  ButtonBar,
  ButtonBarItem,
  Dialog,
  DialogActions,
  DialogTitle,
  Field,
  Select,
  Textarea,
} from '@ctrliq/quantic-react';
import { useForm, useStore } from '@tanstack/react-form';
import { useId, useState } from 'react';

import {
  useCreateRegistrationToken,
  useRegisterKubeconfigCluster,
} from '~/api/fleet/fleetComponents';
import type { RegistrationTokenSecret } from '~/api/fleet/fleetSchemas';
import { toast } from '~/shared/components/Toaster/toast';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { formSubmitHandler } from '~/shared/utils/form';
import { toLocalTime } from '~/shared/utils/formatters';
import { hasFeature } from '~/shared/utils/predicates';

import { CopyableCommand } from '../components/CopyableCommand/CopyableCommand';
import { HintedText } from '../components/HintedField/HintedText';
import {
  agentProfiles,
  lifetimeOptions,
  methodExplanations,
  methodLabels,
  methodSubtitles,
  profileHints,
  profileOptions,
  registerNameHelp,
  registrationMethods,
  tokenLifetimes,
  tokenShownSubtitle,
  unreadableFile,
} from '../constants';
import { useFleetRefresh } from '../hooks/useFleetRefresh';
import { registrationSchema } from '../schemas/registration';
import type { NamedCluster, RegistrationValues } from '../types';
import { toKubeconfigRefusal, toNameHint, toTokenRefusal } from '../utils/formatters';
import { fromChoice } from '../utils/parsers';
import { isRegistrationReady } from '../utils/predicates';
import { toKubeconfigRequest, toTokenRequest } from '../utils/transforms';
import styles from './RegisterClusterDialog.module.scss';

type Props = {
  clusters: NamedCluster[];
  onClosed: () => void;
};

type TokenProps = {
  minted: Partial<RegistrationTokenSecret>;
  clusterName: string;
};

const emptyRegistration: RegistrationValues = {
  method: 'agent',
  name: '',
  displayName: '',
  profile: 'full',
  lifetime: '3600',
  kubeconfig: '',
  context: '',
};

const MintedToken = ({ minted, clusterName }: TokenProps) => (
  <div className={styles.main}>
    <CopyableCommand label="Apply on the member cluster" text={minted.applyCommand ?? ''} />
    <p className={styles.notice}>
      The token is embedded in this command and is shown <strong>once</strong> — it is stored hashed
      on the hub and cannot be retrieved again. It expires{' '}
      <time>{toLocalTime(minted.expiresAt)}</time>. The agent will appear on this page as{' '}
      <code>{clusterName}</code> once it connects.
    </p>
    <p className={styles.notice}>
      Nodes that run executors for fleet operations need the executor&apos;s host wiring: append{' '}
      <code>&amp;executor=true</code> to the manifest URL the fetch reads — an opt-in on the fetch,
      never on the token, because the same token keeps rendering the default bundle for members that
      want no host wiring.
    </p>
    {minted.manifestURL ? <p className={styles.notice}>manifest: {minted.manifestURL}</p> : null}
  </div>
);

export const RegisterClusterDialog = ({ clusters, onClosed }: Props) => {
  const formId = useId();
  const config = useServerConfig();
  const onFleetChanged = useFleetRefresh();
  const [fileError, setFileError] = useState<string>();
  const token = useCreateRegistrationToken({
    onSuccess: (minted) => {
      toast(`Registration token ${minted.name} created`);
    },
  });
  const kubeconfig = useRegisterKubeconfigCluster({
    onSuccess: (_cluster, { body }) => {
      toast(`${body.name} registered`);
      void onFleetChanged();
      onClosed();
    },
  });
  const form = useForm({
    defaultValues: emptyRegistration,
    validators: { onSubmit: registrationSchema },
    onSubmit: ({ value }) => {
      const isReady =
        isRegistrationReady(value, clusters) && !token.isPending && !kubeconfig.isPending;
      setFileError(undefined);
      if (isReady && value.method === 'agent') {
        token.mutate({ body: toTokenRequest(value) });
      } else if (isReady) {
        kubeconfig.mutate({ body: toKubeconfigRequest(value) });
      }
    },
  });
  const values = useStore(form.store, (state) => state.values);
  const isAgent = values.method === 'agent';
  const isPending = token.isPending || kubeconfig.isPending;
  const failure =
    fileError ??
    (token.error
      ? toTokenRefusal(token.error, values.name)
      : kubeconfig.error
        ? toKubeconfigRefusal(kubeconfig.error, values.name)
        : undefined);
  const onMethodChosen = (method: RegistrationValues['method']): void => {
    form.setFieldValue('method', method);
    token.reset();
    kubeconfig.reset();
    setFileError(undefined);
  };
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
        Register an existing cluster
        <span slot="subtitle">
          {token.data ? tokenShownSubtitle : methodSubtitles[values.method]}
        </span>
      </DialogTitle>
      {token.data ? (
        <MintedToken minted={token.data} clusterName={values.name} />
      ) : (
        <form id={formId} className={styles.main} onSubmit={formSubmitHandler(form)}>
          {hasFeature(config, 'kubeconfigSource') ? (
            <section className={styles.method}>
              <ButtonBar aria-label="Registration method">
                {registrationMethods.map((method) => (
                  <ButtonBarItem
                    key={method}
                    active={values.method === method}
                    onClick={() => {
                      onMethodChosen(method);
                    }}
                  >
                    {methodLabels[method]}
                  </ButtonBarItem>
                ))}
              </ButtonBar>
              <p className={styles.notice}>{methodExplanations[values.method]}</p>
            </section>
          ) : null}
          <form.Field name="name">
            {(field) => (
              <HintedText
                label="Cluster name"
                value={field.state.value}
                placeholder="prod-us-east"
                hint={toNameHint(field.state.value, clusters, registerNameHelp)}
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
                placeholder="Production · us-east-1"
                hint={{ text: 'optional — free text shown in the switcher', isError: false }}
                onChanged={field.handleChange}
              />
            )}
          </form.Field>
          {isAgent ? (
            <>
              <form.Field name="profile">
                {(field) => (
                  <Field
                    fieldId={`${formId}-profile`}
                    label="Agent profile"
                    hint={profileHints[field.state.value]}
                  >
                    <Select
                      id={`${formId}-profile`}
                      options={profileOptions}
                      value={field.state.value}
                      onValueChanged={(event) => {
                        field.handleChange(fromChoice(agentProfiles, event.detail.value, 'full'));
                      }}
                    />
                  </Field>
                )}
              </form.Field>
              <form.Field name="lifetime">
                {(field) => (
                  <Field fieldId={`${formId}-lifetime`} label="Token lifetime">
                    <Select
                      id={`${formId}-lifetime`}
                      options={lifetimeOptions}
                      value={field.state.value}
                      onValueChanged={(event) => {
                        field.handleChange(fromChoice(tokenLifetimes, event.detail.value, '3600'));
                      }}
                    />
                  </Field>
                )}
              </form.Field>
            </>
          ) : (
            <>
              <form.Field name="kubeconfig">
                {(field) => (
                  <Field
                    fieldId={`${formId}-kubeconfig`}
                    label="Kubeconfig"
                    hint="the file stays in this browser until you submit; the fleet manager then stores it"
                  >
                    <input
                      type="file"
                      aria-label="Kubeconfig file"
                      accept=".yaml,.yml,.conf,.kubeconfig,text/yaml,text/plain"
                      onChange={(event) => {
                        void event.target.files?.[0]?.text().then(
                          (text) => {
                            field.handleChange(text);
                            setFileError(undefined);
                          },
                          () => {
                            setFileError(unreadableFile);
                          },
                        );
                      }}
                    />
                    <Textarea
                      id={`${formId}-kubeconfig`}
                      aria-label="Kubeconfig contents"
                      rows={7}
                      value={field.state.value}
                      placeholder="…or paste the kubeconfig here"
                      onValueChanged={(event) => {
                        field.handleChange(event.detail.value);
                      }}
                    />
                  </Field>
                )}
              </form.Field>
              <form.Field name="context">
                {(field) => (
                  <HintedText
                    label="Context"
                    value={field.state.value}
                    placeholder="production"
                    hint={{
                      text: "optional — defaults to the file's current-context",
                      isError: false,
                    }}
                    onChanged={field.handleChange}
                  />
                )}
              </form.Field>
              <p className={styles.notice}>
                The fleet manager will hold this credential and use it for every request to the
                cluster, so it has whatever access the kubeconfig grants. It is stored in a Secret
                that the dashboard cannot read back. Credential plugins (<code>exec</code>), file
                paths and <code>insecure-skip-tls-verify</code> are refused.
              </p>
            </>
          )}
          {failure ? (
            <Alert type="error">
              <span slot="title">Registration failed</span>
              {failure}
            </Alert>
          ) : null}
        </form>
      )}
      <DialogActions slot="footer">
        {token.data ? (
          <Button onClick={onClosed}>Done</Button>
        ) : (
          <>
            <Button variant={ButtonVariant.Ghost} onClick={onClosed}>
              Cancel
            </Button>
            <Button
              type="submit"
              form={formId}
              disabled={!isRegistrationReady(values, clusters) || isPending}
            >
              {isAgent
                ? token.isPending
                  ? 'Creating…'
                  : 'Create token'
                : kubeconfig.isPending
                  ? 'Connecting…'
                  : 'Register cluster'}
            </Button>
          </>
        )}
      </DialogActions>
    </Dialog>
  );
};
