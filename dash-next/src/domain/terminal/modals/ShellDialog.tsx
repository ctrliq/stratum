import { Button, Dialog, DialogTitle, Select, TextInput } from '@ctrliq/quantic-react';
import { useForm } from '@tanstack/react-form';
import { useContext, useState } from 'react';

import { useListCoreV1PodForAllNamespaces } from '~/api/k8s/k8sComponents';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { polledWhen } from '~/shared/query/polling';
import { defaultCommand } from '~/shared/streaming/constants';
import type { ExecTarget } from '~/shared/streaming/types';
import { formSubmitHandler, stringFieldProps } from '~/shared/utils/form';

import { ExecTerminal } from '../components/ExecTerminal/ExecTerminal';
import { shellDialogGuide, shellDialogSubtitle, shellDialogTitle } from '../constants';
import { shellSchema, type ShellValues } from '../schemas/shell';
import { fromPodReference } from '../utils/parsers';
import { runningPodsOf } from '../utils/selectors';
import { toPodOptions } from '../utils/transforms';
import styles from './ShellDialog.module.scss';

type Props = {
  isOpen: boolean;
  onClosed: () => void;
};

type Session = {
  target: ExecTarget;
  run: number;
};

function PodShell() {
  const isLive = useContext(LiveRefreshContext);
  const pods = useListCoreV1PodForAllNamespaces(
    {},
    { ...polledWhen(isLive), select: runningPodsOf },
  );
  const running = pods.data ?? [];
  const [session, setSession] = useState<Session>();
  const [defaultValues] = useState<ShellValues>(() => ({
    pod: running[0] ?? '',
    command: defaultCommand,
  }));
  const form = useForm({
    defaultValues,
    validators: { onMount: shellSchema, onChange: shellSchema },
    onSubmit: ({ value }) => {
      setSession((current) => ({
        target: { ...fromPodReference(value.pod), command: value.command },
        run: (current?.run ?? 0) + 1,
      }));
    },
  });

  return (
    <>
      <form className={styles.picker} onSubmit={formSubmitHandler(form)}>
        <form.Field name="pod">
          {(field) => (
            <Select
              className={styles.pod}
              aria-label="Pod"
              name={field.name}
              options={toPodOptions(running, field.state.value)}
              value={field.state.value}
              onValueChanged={(event) => {
                field.handleChange(event.detail.value);
                setSession(undefined);
              }}
            />
          )}
        </form.Field>
        <form.Field name="command">
          {(field) => <TextInput aria-label="Command" {...stringFieldProps(field)} />}
        </form.Field>
        <form.Subscribe selector={(state) => state.canSubmit}>
          {(canSubmit) => (
            <Button type="submit" disabled={!canSubmit}>
              Connect
            </Button>
          )}
        </form.Subscribe>
      </form>
      {session ? (
        <div className={styles.terminal}>
          <ExecTerminal key={session.run} target={session.target} />
        </div>
      ) : (
        <p className={styles.guide}>{shellDialogGuide}</p>
      )}
    </>
  );
}

export function ShellDialog({ isOpen, onClosed }: Props) {
  return (
    <Dialog
      open={isOpen}
      size="lg"
      onOpenChanged={(event) => {
        if (!event.detail.open) {
          onClosed();
        }
      }}
    >
      <DialogTitle slot="header">
        {shellDialogTitle}
        <span slot="subtitle">{shellDialogSubtitle}</span>
      </DialogTitle>
      {isOpen ? <PodShell /> : null}
    </Dialog>
  );
}
