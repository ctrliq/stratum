import { Field, TextInput } from '@ctrliq/quantic-react';
import type { AnyFieldApi } from '@tanstack/react-form';
import type { ComponentProps } from 'react';

type Props = {
  field: AnyFieldApi;
  label: string;
  type: 'text' | 'password';
  autoComplete: ComponentProps<typeof TextInput>['autoComplete'];
  hint?: string;
  isLocked: boolean;
};

const valueOf = (field: AnyFieldApi): string =>
  typeof field.state.value === 'string' ? field.state.value : '';

export const CredentialField = ({ field, label, type, autoComplete, hint, isLocked }: Props) => {
  const fieldId = `sign-in-${field.name}`;
  return (
    <Field fieldId={fieldId} label={label} hint={hint}>
      <TextInput
        id={fieldId}
        name={field.name}
        type={type}
        autoComplete={autoComplete}
        value={valueOf(field)}
        disabled={isLocked}
        onValueChanged={(event) => {
          field.handleChange(event.detail.value);
        }}
        onBlur={field.handleBlur}
      />
    </Field>
  );
};
