import { FieldValidationStatus } from '@ctrliq/quantic-components';
import {
  Button,
  Checkbox,
  CodeInput,
  Field,
  Fieldset,
  Spinner,
  Textarea,
  TextInput,
} from '@ctrliq/quantic-react';
import { useForm } from '@tanstack/react-form';
import { useId, useState } from 'react';

import { toYaml } from '~/shared/utils/formatters';

import { AppDetailFrame } from '../components/AppDetailFrame';
import { AppSummary } from '../components/AppSummary';
import { catalogNamespace, secretManifestCaption, valuesPlaceholder } from '../constants';
import { brokenRulesOf, isInstallable, toInstallSchema } from '../schemas/install';
import type { CatalogApp, InstallValues } from '../types/catalog';
import { toFooterCaption, toRequiredLine, toSecretHint } from '../utils/formatters';
import { isSameValues } from '../utils/predicates';
import { credentialsOf, editableParamsOf, unmetParamsOf } from '../utils/selectors';
import {
  toHelmChart,
  toParamGroups,
  toPreviewChart,
  toSecretManifest,
  toSeedValues,
} from '../utils/transforms';
import styles from './InstallForm.module.scss';
import { ParamField } from './ParamField';

type Props = {
  app: CatalogApp;
  initialValues: InstallValues;
  isStateKnown: boolean;
  canWrite: boolean;
  isBusy: boolean;
  isInstalling: boolean;
  onInstalled: (values: InstallValues) => void;
  onReset: () => void;
  onClosed: () => void;
};

const toRuleProps = (rule: string | undefined) =>
  rule === undefined ? {} : { status: FieldValidationStatus.Error, message: rule };

export const InstallForm = ({
  app,
  initialValues,
  isStateKnown,
  canWrite,
  isBusy,
  isInstalling,
  onInstalled,
  onReset,
  onClosed,
}: Props) => {
  const fieldId = useId();
  const [isPreviewShown, setIsPreviewShown] = useState(false);
  const seed = toSeedValues(app);
  const credentials = credentialsOf(app);
  const form = useForm({
    defaultValues: initialValues,
    validators: { onSubmit: toInstallSchema(app) },
    onSubmit: ({ value }) => {
      onInstalled(value);
    },
  });

  return (
    <form.Subscribe selector={(state) => state.values}>
      {(values) => {
        const rules = brokenRulesOf(app, values);
        const missing = unmetParamsOf(app, values);
        const secretName = values.credentialsSecret.trim();
        return (
          <AppDetailFrame
            app={app}
            onClosed={onClosed}
            footer={
              <>
                <p className={styles.caption}>{toFooterCaption(app, isStateKnown)}</p>
                {canWrite ? (
                  <Button
                    size="xs"
                    disabled={isBusy || !isStateKnown || !isInstallable(app, values)}
                    onClick={() => void form.handleSubmit()}
                  >
                    {isInstalling ? <Spinner size="xs" label="Installing" /> : null}
                    {isInstalling ? 'Installing…' : 'Install'}
                  </Button>
                ) : null}
              </>
            }
          >
            <AppSummary app={app} targetNamespace={values.targetNamespace} />
            <form
              className={styles.main}
              aria-labelledby={`${fieldId}-heading`}
              onSubmit={(event) => {
                event.preventDefault();
                void form.handleSubmit();
              }}
            >
              <header className={styles.heading}>
                <h3 id={`${fieldId}-heading`} className={styles.title}>
                  Install
                </h3>
                {isSameValues(seed, values) ? null : (
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => {
                      form.reset(seed);
                      onReset();
                    }}
                  >
                    Reset
                  </Button>
                )}
              </header>
              <form.Field name="releaseName">
                {(field) => (
                  <Field
                    fieldId={`${fieldId}-release`}
                    label="Release name"
                    {...toRuleProps(rules.releaseName)}
                  >
                    <TextInput
                      id={`${fieldId}-release`}
                      name={field.name}
                      value={field.state.value}
                      invalid={rules.releaseName !== undefined}
                      onValueChanged={(event) => {
                        field.handleChange(event.detail.value);
                      }}
                      onBlur={field.handleBlur}
                    />
                  </Field>
                )}
              </form.Field>
              <form.Field name="targetNamespace">
                {(field) => (
                  <Field
                    fieldId={`${fieldId}-namespace`}
                    label="Target namespace"
                    {...toRuleProps(rules.targetNamespace)}
                  >
                    <TextInput
                      id={`${fieldId}-namespace`}
                      name={field.name}
                      value={field.state.value}
                      invalid={rules.targetNamespace !== undefined}
                      onValueChanged={(event) => {
                        field.handleChange(event.detail.value);
                      }}
                      onBlur={field.handleBlur}
                    />
                  </Field>
                )}
              </form.Field>
              <form.Field name="createNamespace">
                {(field) => (
                  <Checkbox
                    name={field.name}
                    label="Create the namespace if it does not exist"
                    checked={field.state.value}
                    onValueChanged={(event) => {
                      field.handleChange(event.detail.checked);
                    }}
                  />
                )}
              </form.Field>
              <form.Field name="params">
                {(field) =>
                  toParamGroups(editableParamsOf(app)).map(({ group, params }) => {
                    const editors = params.map((param) => (
                      <ParamField
                        key={param.key}
                        fieldId={`${fieldId}-param-${param.key}`}
                        param={param}
                        value={field.state.value[param.key]}
                        onChanged={(value) => {
                          field.handleChange({ ...field.state.value, [param.key]: value });
                        }}
                      />
                    ));
                    return group ? (
                      <Fieldset key={group} legend={group} variant="bare">
                        {editors}
                      </Fieldset>
                    ) : (
                      editors
                    );
                  })
                }
              </form.Field>
              {credentials.length > 0 ? (
                <form.Field name="credentialsSecret">
                  {(field) => (
                    <section className={styles.credentials} aria-label="Credentials">
                      <Field
                        fieldId={`${fieldId}-secret`}
                        label="Credentials Secret"
                        hint={
                          rules.credentialsSecret === undefined ? toSecretHint(credentials) : ''
                        }
                        {...toRuleProps(rules.credentialsSecret)}
                      >
                        <TextInput
                          id={`${fieldId}-secret`}
                          name={field.name}
                          value={field.state.value}
                          invalid={rules.credentialsSecret !== undefined}
                          onValueChanged={(event) => {
                            field.handleChange(event.detail.value);
                          }}
                          onBlur={field.handleBlur}
                        />
                      </Field>
                      {secretName !== '' && rules.credentialsSecret === undefined ? (
                        <Field fieldId={`${fieldId}-manifest`} label={secretManifestCaption}>
                          <CodeInput
                            id={`${fieldId}-manifest`}
                            value={toSecretManifest(app, secretName)}
                            readOnly
                          />
                        </Field>
                      ) : null}
                      <p className={styles.note}>
                        The values never pass through this dashboard and are not stored in the
                        HelmChart, which names the Secret in <code>spec.valuesSecrets</code>; anyone
                        who can read Secrets in {catalogNamespace}, hub viewers included, can still
                        read them there. A Stratum operator that predates that field ignores it, and
                        the chart&apos;s defaults apply.
                      </p>
                    </section>
                  )}
                </form.Field>
              ) : null}
              <details className={styles.advanced}>
                <summary>ADVANCED VALUES</summary>
                <form.Field name="values">
                  {(field) => (
                    <Textarea
                      aria-label="values.yaml"
                      name={field.name}
                      rows={8}
                      placeholder={valuesPlaceholder}
                      value={field.state.value}
                      onValueChanged={(event) => {
                        field.handleChange(event.detail.value);
                      }}
                      onBlur={field.handleBlur}
                    />
                  )}
                </form.Field>
                <p className={styles.note}>
                  Passed verbatim as <code>spec.valuesContent</code> (YAML, not validated). Form
                  fields above take precedence (<code>helm --set</code>).
                </p>
              </details>
              <Button
                className={styles.previewControl}
                variant="ghost"
                size="sm"
                onClick={() => {
                  setIsPreviewShown((isShown) => !isShown);
                }}
              >
                {isPreviewShown ? 'HIDE MANIFEST' : 'PREVIEW MANIFEST'}
              </Button>
              {isPreviewShown ? (
                <CodeInput
                  aria-label="HelmChart to create"
                  value={toYaml(toPreviewChart(app, toHelmChart(app, values)))}
                  readOnly
                  rows={16}
                />
              ) : null}
              {missing.length > 0 ? (
                <p className={styles.required}>{toRequiredLine(missing)}</p>
              ) : null}
            </form>
          </AppDetailFrame>
        );
      }}
    </form.Subscribe>
  );
};
