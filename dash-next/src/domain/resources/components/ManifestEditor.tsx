import { CodeInput, Field } from '@ctrliq/quantic-react';
import type { ComponentProps } from 'react';

import styles from './ManifestEditor.module.scss';

type Props = {
  fieldId: string;
  caption: string;
  label: string;
} & Pick<ComponentProps<typeof CodeInput>, 'name' | 'value' | 'onValueChanged' | 'onBlur'>;

export function ManifestEditor({ fieldId, caption, label, ...fieldProps }: Props) {
  return (
    <Field className={styles.main} fieldId={fieldId} label={caption}>
      <CodeInput
        id={fieldId}
        className={styles.editor}
        aria-label={label}
        lineNumbers
        stretch
        {...fieldProps}
      />
    </Field>
  );
}
