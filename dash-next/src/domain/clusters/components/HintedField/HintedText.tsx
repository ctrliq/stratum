import { TextInput } from '@ctrliq/quantic-react';
import { useId } from 'react';

import type { FieldHint } from '../../types';
import { HintedField } from './HintedField';

type Props = {
  label: string;
  value: string;
  hint?: FieldHint;
  placeholder?: string;
  onChanged: (value: string) => void;
};

export const HintedText = ({ label, value, hint, placeholder, onChanged }: Props) => {
  const fieldId = useId();
  return (
    <HintedField fieldId={fieldId} label={label} hint={hint}>
      <TextInput
        id={fieldId}
        value={value}
        placeholder={placeholder}
        invalid={hint?.isError ?? false}
        onValueChanged={(event) => {
          onChanged(event.detail.value);
        }}
      />
    </HintedField>
  );
};
