import { ActionGroup, Button, Field, Tag, TagGroup, TextInput } from '@ctrliq/quantic-react';
import { useForm } from '@tanstack/react-form';
import { useId, useState } from 'react';

import type { KubeObject } from '~/shared/types/kubernetes';
import { formSubmitHandler, stringFieldProps } from '~/shared/utils/form';
import { toDisplayText } from '~/shared/utils/formatters';
import { nameOf, namespaceOf, uidOf } from '~/shared/utils/selectors';
import { toLabelPairs } from '~/shared/utils/transforms';

import { editCaption, replicasWhenUnstated } from '../constants';
import { scaleSchema } from '../schemas/scale';
import type { BoundAction, DetailMode, ResourceKind } from '../types/resources';
import { toCreatedText, toManifestName } from '../utils/formatters';
import { fromReplicaText } from '../utils/parsers';
import { toManifestText } from '../utils/transforms';
import { ManifestEditor } from './ManifestEditor';
import { ManifestView } from './ManifestView';
import styles from './ResourceDetail.module.scss';
import { ResourcePanel } from './ResourcePanel';

export type ResourceDetailProps = {
  kind: ResourceKind;
  object: KubeObject;
  age: string;
  actions: BoundAction[];
  isScalable: boolean;
  replicas: number | undefined;
  hasWriteControls: boolean;
  isRestartable: boolean;
  isWriting: boolean;
  onApplied: (text: string) => void;
  onScaled: (replicas: number | null) => void;
  onDeleteRequested: () => void;
  onRestartRequested: () => void;
  onClosed: () => void;
};

type ScaleValues = { replicas: string };

export function ResourceDetail({
  kind,
  object,
  age,
  actions,
  isScalable,
  replicas,
  hasWriteControls,
  isRestartable,
  isWriting,
  onApplied,
  onScaled,
  onDeleteRequested,
  onRestartRequested,
  onClosed,
}: ResourceDetailProps) {
  const [mode, setMode] = useState<DetailMode>('view');
  const fieldId = useId();
  const labels = toLabelPairs(object);
  const scaleValues: ScaleValues = { replicas: String(replicas ?? replicasWhenUnstated) };

  const editForm = useForm({
    defaultValues: { manifest: toManifestText(object) },
    onSubmit: ({ value }) => onApplied(value.manifest),
  });

  const scaleForm = useForm({
    defaultValues: scaleValues,
    validators: { onChange: scaleSchema },
    onSubmit: ({ value }) => onScaled(fromReplicaText(value.replicas)),
    onSubmitInvalid: ({ value }) => onScaled(fromReplicaText(value.replicas)),
  });

  const onViewRestored = (): void => {
    editForm.reset();
    scaleForm.reset();
    setMode('view');
  };

  return (
    <ResourcePanel
      title={nameOf(object)}
      subtitle={kind.kind}
      onClosed={onClosed}
      footer={
        actions.length > 0 || hasWriteControls ? (
          <>
            <ActionGroup aria-label={`Actions for ${nameOf(object)}`}>
              {actions.map((action) => (
                <Button size="xs" key={action.title} variant="outline" onClick={action.onRun}>
                  {action.title}
                </Button>
              ))}
              {hasWriteControls ? (
                <Button size="xs" variant="outline" destructive onClick={onDeleteRequested}>
                  Delete
                </Button>
              ) : null}
              {hasWriteControls && isRestartable ? (
                <Button size="xs" variant="outline" onClick={onRestartRequested}>
                  Restart
                </Button>
              ) : null}
              {hasWriteControls && isScalable && mode !== 'scale' ? (
                <Button size="xs" variant="outline" onClick={() => setMode('scale')}>
                  Scale
                </Button>
              ) : null}
            </ActionGroup>
            <ActionGroup aria-label="Panel">
              {mode === 'view' && hasWriteControls ? (
                <Button size="xs" variant="secondary" onClick={() => setMode('edit')}>
                  Edit
                </Button>
              ) : null}
              {mode === 'view' ? null : (
                <Button size="xs" variant="outline" onClick={onViewRestored}>
                  Cancel
                </Button>
              )}
              {mode === 'edit' ? (
                <Button size="xs" disabled={isWriting} onClick={() => void editForm.handleSubmit()}>
                  Apply
                </Button>
              ) : null}
              {mode === 'scale' ? (
                <scaleForm.Subscribe selector={(state) => state.canSubmit}>
                  {(canSubmit) => (
                    <Button
                      size="xs"
                      disabled={isWriting || !canSubmit}
                      onClick={() => void scaleForm.handleSubmit()}
                    >
                      Scale
                    </Button>
                  )}
                </scaleForm.Subscribe>
              ) : null}
            </ActionGroup>
          </>
        ) : null
      }
    >
      <dl className={styles.summary}>
        <dt>Kind</dt>
        <dd>{kind.kind}</dd>
        {kind.isNamespaced ? (
          <>
            <dt>Namespace</dt>
            <dd>{toDisplayText(namespaceOf(object))}</dd>
          </>
        ) : null}
        <dt>Created</dt>
        <dd>{toCreatedText(age)}</dd>
        <dt>UID</dt>
        <dd>{toDisplayText(uidOf(object))}</dd>
      </dl>
      {labels.length > 0 ? (
        <TagGroup aria-label="Labels">
          {labels.map((label) => (
            <Tag key={label} size="sm">
              {label}
            </Tag>
          ))}
        </TagGroup>
      ) : null}
      {mode === 'view' ? (
        <ManifestView object={object} label={toManifestName(kind, object)} />
      ) : null}
      {mode === 'edit' ? (
        <form className={styles.editing} onSubmit={formSubmitHandler(editForm)}>
          <editForm.Field name="manifest">
            {(field) => (
              <ManifestEditor
                fieldId={`${fieldId}-manifest`}
                caption={editCaption}
                label={toManifestName(kind, object)}
                {...stringFieldProps(field)}
              />
            )}
          </editForm.Field>
        </form>
      ) : null}
      {mode === 'scale' ? (
        <form onSubmit={formSubmitHandler(scaleForm)}>
          <scaleForm.Field name="replicas">
            {(field) => (
              <Field fieldId={`${fieldId}-replicas`} label="Desired replicas">
                <TextInput
                  id={`${fieldId}-replicas`}
                  inputMode="numeric"
                  {...stringFieldProps(field)}
                />
              </Field>
            )}
          </scaleForm.Field>
        </form>
      ) : null}
    </ResourcePanel>
  );
}
