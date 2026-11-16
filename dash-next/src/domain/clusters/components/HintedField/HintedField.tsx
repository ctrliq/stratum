import { FieldValidationStatus } from '@ctrliq/quantic-components';
import { Field } from '@ctrliq/quantic-react';
import type { ReactNode } from 'react';

import type { FieldHint } from '../../types';

type Props = {
  fieldId: string;
  label: string;
  hint?: FieldHint;
  children: ReactNode;
};

export const HintedField = ({ fieldId, label, hint, children }: Props) =>
  hint?.isError ? (
    <Field fieldId={fieldId} label={label} status={FieldValidationStatus.Error} message={hint.text}>
      {children}
    </Field>
  ) : (
    <Field fieldId={fieldId} label={label} hint={hint?.text}>
      {children}
    </Field>
  );
