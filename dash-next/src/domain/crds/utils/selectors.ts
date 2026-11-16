import type {
  CustomResourceDefinition,
  CustomResourceDefinitionVersion,
} from '~/api/k8s/k8sSchemas';
import { isRecord } from '~/shared/utils/predicates';

import { fallbackVersion, numericTypes } from '../constants';
import type { InstanceAddress, PathStep, PrinterColumn } from '../types/crds';
import { fromColumnPath } from './parsers';
import { isIdentityPath, isShownColumn } from './predicates';

const isPresent = (value: unknown): boolean => value !== undefined && value !== null;

const dugFrom = (value: unknown, path: string[]): unknown =>
  path.reduce<unknown>((found, key) => (isRecord(found) ? found[key] : undefined), value);

const steppedFrom = (value: unknown, step: PathStep): unknown[] =>
  step.type === 'field'
    ? isRecord(value)
      ? [value[step.name]]
      : []
    : step.type === 'index'
      ? Array.isArray(value)
        ? [value.at(step.index)]
        : []
      : step.type === 'all'
        ? Array.isArray(value)
          ? value
          : isRecord(value)
            ? Object.values(value)
            : []
        : Array.isArray(value)
          ? value.filter((element: unknown) => {
              const found = dugFrom(element, step.path);
              return found !== undefined && (String(found) === step.value) === step.isEqual;
            })
          : [];

export const valuesAt = (object: unknown, steps: PathStep[] | undefined): unknown[] =>
  (steps ?? [])
    .reduce<unknown[]>(
      (values, step) => values.filter(isPresent).flatMap((value) => steppedFrom(value, step)),
      steps ? [object] : [],
    )
    .filter(isPresent);

export const versionsOf = ({ spec }: CustomResourceDefinition): CustomResourceDefinitionVersion[] =>
  spec.versions;

export const servedVersionsOf = (definition: CustomResourceDefinition | undefined): string[] =>
  definition
    ? versionsOf(definition)
        .filter(({ served }) => served)
        .map(({ name }) => name)
    : [];

export const firstServedVersionOf = (definition: CustomResourceDefinition): string => {
  const versions = versionsOf(definition);
  return (versions.find(({ served }) => served) ?? versions[0])?.name ?? fallbackVersion;
};

export const addressOf = (definition: CustomResourceDefinition): InstanceAddress => ({
  group: definition.spec.group,
  version: firstServedVersionOf(definition),
  plural: definition.spec.names.plural,
});

export const printerColumnsOf = (
  definition: CustomResourceDefinition | undefined,
  version: string,
): PrinterColumn[] =>
  (
    (definition ? versionsOf(definition) : []).find(({ name }) => name === version)
      ?.additionalPrinterColumns ?? []
  )
    .filter(isShownColumn)
    .map((column) => ({ column, steps: fromColumnPath(column.jsonPath) }))
    .filter(({ steps }) => !isIdentityPath(steps))
    .map(({ column, steps }, index) => ({
      id: `printer-${index}`,
      name: column.name,
      type: column.type || 'string',
      description: column.description || undefined,
      steps,
      isNumeric: numericTypes.includes(column.type),
    }));
