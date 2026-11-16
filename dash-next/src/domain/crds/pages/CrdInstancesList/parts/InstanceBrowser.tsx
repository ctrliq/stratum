import { CellAlign } from '@ctrliq/quantic-components';
import type { ReactNode } from 'react';

import type { CustomResourceDefinition } from '~/api/k8s/k8sSchemas';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';
import { useServerConfig } from '~/shared/context/ConfigContext';
import {
  useCreateArbitraryObject,
  useDeleteArbitraryObject,
  useListArbitraryKind,
  useReplaceArbitraryObject,
} from '~/shared/hooks/useArbitraryKind';
import { toDisplayText } from '~/shared/utils/formatters';

import type { InstanceAddress, InstanceColumns, PrinterColumn } from '../../../types/crds';
import { hasUnresolvedNamespace, isHubManaged } from '../../../utils/predicates';
import { printerColumnsOf } from '../../../utils/selectors';
import { toInstanceCells, toInstanceKind, toReference } from '../../../utils/transforms';
import { HintedText } from './HintedText';

type Props = {
  definition: CustomResourceDefinition | undefined;
  address: InstanceAddress;
  linkedName?: string;
  tags: ReactNode;
};

const toTableColumns = (columns: PrinterColumn[]): InstanceColumns =>
  columns.map(({ id, name, description, isNumeric }, index) => ({
    id,
    header: () => <HintedText text={name} hint={description} />,
    cell: ({ row }) => (
      <HintedText
        text={row.original.printed[index]?.text ?? ''}
        hint={row.original.printed[index]?.fullText}
      />
    ),
    meta: { align: isNumeric ? CellAlign.Right : CellAlign.Left },
  }));

const namespaceColumn: InstanceColumns[number] = {
  id: 'namespace',
  header: 'Namespace',
  accessorFn: (row) => toDisplayText(row.namespace),
};

export function InstanceBrowser({ definition, address, linkedName, tags }: Props) {
  const config = useServerConfig();
  const list = useListArbitraryKind(address, resourceListQuery);
  const { mutateAsync: created } = useCreateArbitraryObject(address);
  const { mutateAsync: replaced } = useReplaceArbitraryObject(address);
  const { mutateAsync: deleted } = useDeleteArbitraryObject(address);
  const columns = printerColumnsOf(definition, address.version);
  const kind = toInstanceKind(definition, address, list.data?.items ?? []);

  return (
    <ResourceBrowser
      kind={kind}
      list={list}
      columns={
        hasUnresolvedNamespace(definition, kind)
          ? [namespaceColumn, ...toTableColumns(columns)]
          : toTableColumns(columns)
      }
      toRow={toInstanceCells(columns, list.dataUpdatedAt)}
      linkedName={linkedName}
      tags={tags}
      hasIdentityColumns={definition !== undefined}
      isReadOnly={isHubManaged(config, address)}
      writes={{
        onCreate: (namespace, body) => created({ namespace: namespace || undefined, body }),
        onReplace: (target, body) => replaced({ reference: toReference(target), body }),
        onDelete: (target) => deleted(toReference(target)),
      }}
    />
  );
}
