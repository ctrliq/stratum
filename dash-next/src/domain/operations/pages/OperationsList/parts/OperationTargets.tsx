import { useMemo } from 'react';

import type { Operation } from '~/api/fleet/fleetSchemas';
import { DataTable, type DataTableColumns } from '~/shared/components/DataTable/DataTable';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';

import type { TargetRow } from '../../../types';
import { toOutcomeTone } from '../../../utils/formatters';
import { targetsOf } from '../../../utils/selectors';
import { toTargetRow } from '../../../utils/transforms';

type Props = {
  operation: Operation;
};

const columns: DataTableColumns<TargetRow> = [
  { id: 'target', header: 'Target', accessorKey: 'cluster', meta: { isRowHeader: true } },
  { id: 'node', header: 'Node', accessorKey: 'node' },
  { id: 'state', header: 'State', accessorKey: 'state' },
  {
    id: 'outcome',
    header: 'Outcome',
    cell: ({ row }) => (
      <ToneBadge tone={toOutcomeTone(row.original.outcome)}>{row.original.outcome}</ToneBadge>
    ),
  },
  { id: 'committed', header: 'Committed', accessorKey: 'committed' },
  { id: 'completed', header: 'Completed', accessorKey: 'completed' },
];

export const OperationTargets = ({ operation }: Props) => {
  const rows = useMemo(() => targetsOf(operation).map(toTargetRow), [operation]);
  return <DataTable data={rows} columns={columns} isSortable={false} empty={null} />;
};
