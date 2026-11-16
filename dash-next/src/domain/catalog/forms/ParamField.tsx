import { Checkbox, Field, NumberInput, Select, TextInput } from '@ctrliq/quantic-react';

import type { CatalogParam, ParamValue } from '../types/catalog';
import { toParamLabel } from '../utils/formatters';
import styles from './ParamField.module.scss';

type Props = {
  fieldId: string;
  param: CatalogParam;
  value: ParamValue | undefined;
  onChanged: (value: ParamValue) => void;
};

const enumPlaceholder = 'Select…';

export const ParamField = ({ fieldId, param, value, onChanged }: Props) =>
  param.type === 'boolean' ? (
    <div className={styles.tickBox}>
      <Checkbox
        name={param.key}
        label={param.label}
        checked={value === true}
        onValueChanged={(event) => {
          onChanged(event.detail.checked);
        }}
      />
      {param.help ? <p className={styles.help}>{param.help}</p> : null}
    </div>
  ) : (
    <Field fieldId={fieldId} label={toParamLabel(param)} hint={param.help}>
      {param.type === 'number' ? (
        <NumberInput
          id={fieldId}
          name={param.key}
          value={typeof value === 'number' ? value : null}
          onValueChanged={(event) => {
            onChanged(event.detail.value);
          }}
        />
      ) : param.type === 'enum' ? (
        <Select
          id={fieldId}
          name={param.key}
          placeholder={enumPlaceholder}
          options={param.options.map((option): [string, string] => [option, option])}
          value={typeof value === 'string' ? value : ''}
          onValueChanged={(event) => {
            onChanged(event.detail.value);
          }}
        />
      ) : (
        <TextInput
          id={fieldId}
          name={param.key}
          value={typeof value === 'string' ? value : ''}
          onValueChanged={(event) => {
            onChanged(event.detail.value);
          }}
        />
      )}
    </Field>
  );
