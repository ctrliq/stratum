import { FieldValidationStatus } from '@ctrliq/quantic-components';
import type { AnyFieldApi } from '@tanstack/react-form';

const firstErrorString = (errors: string[] | { message: string }[]): string | undefined => {
  const first = errors[0];
  if (!first) return undefined;
  return typeof first === 'string' ? first : first.message;
};

const resolveError = (meta: AnyFieldApi['state']['meta']): string | undefined => {
  const hasSubmitError = (meta.errorMap.onSubmit?.length ?? 0) > 0;
  if (!meta.isBlurred && !hasSubmitError) return undefined;
  const onChangeErrors = meta.errorMap.onChange;
  if (onChangeErrors !== undefined) return firstErrorString(onChangeErrors);
  if (meta.isDirty) return undefined;
  return firstErrorString(meta.errors);
};

export const lazyValidators = <T>(schema: T) => ({
  onChange: schema,
  onBlur: schema,
  onSubmit: schema,
});

export const fieldValidationProps = (field: AnyFieldApi, id: string) => {
  const errorMessage = resolveError(field.state.meta);
  return {
    fieldId: id,
    ...(errorMessage ? { status: FieldValidationStatus.Error, message: errorMessage } : {}),
  };
};

export const stringFieldProps = (field: AnyFieldApi) => ({
  name: field.name,
  value: field.state.value as string,
  invalid: !!resolveError(field.state.meta),
  onValueChanged: (e: { detail: { value: string } }) => field.handleChange(e.detail.value),
  onBlur: () => field.handleBlur(),
});

export const formSubmitHandler =
  (form: { handleSubmit: () => Promise<void> }) =>
  (e: { preventDefault: () => void }): void => {
    e.preventDefault();
    void form.handleSubmit();
  };
