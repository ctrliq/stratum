import type { CustomResourceDefinition } from '~/api/k8s/k8sSchemas';
import type { ResourceKind, WriteTarget } from '~/domain/resources/types/resources';
import type { ArbitraryObject } from '~/shared/fetchers/arbitraryKinds';
import type { ObjectReference } from '~/shared/types/kubernetes';
import { absentText, toAge, toDisplayText } from '~/shared/utils/formatters';
import { namespaceOf } from '~/shared/utils/selectors';

import { longestCellText, namespacedScope } from '../constants';
import type {
  DefinitionCells,
  InstanceAddress,
  InstanceCells,
  PrinterCell,
  PrinterColumn,
} from '../types/crds';
import { toApiVersion, toGuessedKind, toUnresolvedTitle } from './formatters';
import { valuesAt, versionsOf } from './selectors';

export const toDefinitionRow = (definition: CustomResourceDefinition): DefinitionCells => ({
  group: toDisplayText(definition.spec.group),
  kind: toDisplayText(definition.spec.names.kind),
  scope: toDisplayText(definition.spec.scope),
  versions: versionsOf(definition)
    .map(({ name }) => name)
    .join(', '),
});

const toValueText = (value: unknown): string =>
  typeof value === 'object' ? JSON.stringify(value) : String(value);

const toCutCell = (text: string): PrinterCell =>
  text.length > longestCellText
    ? { text: `${text.slice(0, longestCellText)}…`, fullText: text }
    : { text };

export const toPrinterCell = (
  object: unknown,
  { steps, type, isNumeric }: Pick<PrinterColumn, 'steps' | 'type' | 'isNumeric'>,
  now: number,
): PrinterCell => {
  const texts = valuesAt(object, steps).map(toValueText);
  return texts.length === 0
    ? { text: absentText }
    : type === 'date'
      ? { text: toAge(texts[0], now) }
      : isNumeric
        ? { text: texts.join(', ') }
        : toCutCell(texts.join(', '));
};

export const toInstanceCells =
  (columns: PrinterColumn[], now: number) =>
  (object: ArbitraryObject): InstanceCells => ({
    printed: columns.map((column) => toPrinterCell(object, column, now)),
  });

export const toInstanceKind = (
  definition: CustomResourceDefinition | undefined,
  address: InstanceAddress,
  instances: ArbitraryObject[],
): ResourceKind =>
  definition
    ? {
        apiVersion: toApiVersion(address),
        kind: definition.spec.names.kind || address.plural,
        plural: definition.spec.names.kind || address.plural,
        isNamespaced: definition.spec.scope === namespacedScope,
      }
    : {
        apiVersion: toApiVersion(address),
        kind: toGuessedKind(address.plural),
        plural: toUnresolvedTitle(address),
        isNamespaced: instances.some((instance) => Boolean(namespaceOf(instance))),
      };

export const toReference = ({ namespace, name }: WriteTarget): ObjectReference => ({
  namespace: namespace || undefined,
  name,
});
