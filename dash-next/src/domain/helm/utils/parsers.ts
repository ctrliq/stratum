import type { SecretSummary } from '~/api/dashboard/dashboardSchemas';
import type { KubeObject } from '~/shared/types/kubernetes';
import { isRecord } from '~/shared/utils/predicates';

import {
  releaseNameLabel,
  releaseRevisionLabel,
  releaseSecretType,
  releaseStatusLabel,
  requiredTableColumns,
} from '../constants';

export const fromRevisionLabel = (text: string | undefined): number =>
  Number.parseInt(text ?? '', 10) || 0;

const fromTableRow = (row: unknown, columnCount: number): KubeObject | undefined =>
  isRecord(row) &&
  Array.isArray(row.cells) &&
  row.cells.length === columnCount &&
  isRecord(row.object) &&
  isRecord(row.object.metadata)
    ? { metadata: row.object.metadata }
    : undefined;

export const fromSecretTable = (value: unknown): KubeObject[] | undefined => {
  const columns =
    isRecord(value) && Array.isArray(value.columnDefinitions)
      ? value.columnDefinitions.map((column: unknown) =>
          isRecord(column) ? column.name : undefined,
        )
      : [];
  const rows = isRecord(value) && Array.isArray(value.rows) ? value.rows : undefined;
  const secrets = (rows ?? []).map((row) => fromTableRow(row, columns.length));
  return rows !== undefined &&
    requiredTableColumns.every((name) => columns.includes(name)) &&
    secrets.every((secret) => secret !== undefined)
    ? secrets.filter((secret) => secret !== undefined)
    : undefined;
};

const isReleaseSummary = (value: unknown): value is SecretSummary =>
  isRecord(value) &&
  typeof value.name === 'string' &&
  typeof value.namespace === 'string' &&
  typeof value.type === 'string' &&
  (value.helm === undefined ? value.type !== releaseSecretType : isRecord(value.helm));

const fromSecretSummary = ({
  name,
  namespace,
  creationTimestamp,
  helm,
}: SecretSummary): KubeObject => ({
  metadata: {
    name,
    namespace,
    creationTimestamp: creationTimestamp ?? undefined,
    labels: {
      ...(helm?.name ? { [releaseNameLabel]: helm.name } : {}),
      ...(helm?.status ? { [releaseStatusLabel]: helm.status } : {}),
      ...(helm?.version ? { [releaseRevisionLabel]: helm.version } : {}),
    },
  },
});

export const fromSecretSummaries = (value: unknown): KubeObject[] | undefined =>
  isRecord(value) && Array.isArray(value.items) && value.items.every(isReleaseSummary)
    ? value.items.filter(({ type }) => type === releaseSecretType).map(fromSecretSummary)
    : undefined;
