import { ActionGroup, Button } from '@ctrliq/quantic-react';
import { useForm } from '@tanstack/react-form';
import { useId } from 'react';

import { formSubmitHandler, stringFieldProps } from '~/shared/utils/form';

import type { ResourceKind } from '../types/resources';
import { toCreateCaption, toCreateTitle, toNewManifestName } from '../utils/formatters';
import { ManifestEditor } from './ManifestEditor';
import styles from './ResourceCreate.module.scss';
import { ResourcePanel } from './ResourcePanel';

type Props = {
  kind: ResourceKind;
  skeleton: string;
  isWriting: boolean;
  onCreated: (text: string) => void;
  onClosed: () => void;
};

export function ResourceCreate({ kind, skeleton, isWriting, onCreated, onClosed }: Props) {
  const fieldId = useId();
  const form = useForm({
    defaultValues: { manifest: skeleton },
    onSubmit: ({ value }) => onCreated(value.manifest),
  });

  return (
    <ResourcePanel
      title={toCreateTitle(kind)}
      onClosed={onClosed}
      footer={
        <ActionGroup className={styles.actions} aria-label={toCreateTitle(kind)}>
          <Button size="xs" variant="outline" onClick={onClosed}>
            Cancel
          </Button>
          <Button size="xs" disabled={isWriting} onClick={() => void form.handleSubmit()}>
            Create
          </Button>
        </ActionGroup>
      }
    >
      <form className={styles.main} onSubmit={formSubmitHandler(form)}>
        <form.Field name="manifest">
          {(field) => (
            <ManifestEditor
              fieldId={`${fieldId}-manifest`}
              caption={toCreateCaption(kind)}
              label={toNewManifestName(kind)}
              {...stringFieldProps(field)}
            />
          )}
        </form.Field>
      </form>
    </ResourcePanel>
  );
}
