import {
  Button,
  LucideIconBan,
  LucideIconCircleCheck,
  LucideIconSquareTerminal,
} from '@ctrliq/quantic-react';

import { DataTable, type DataTableColumns } from '~/shared/components/DataTable/DataTable';
import { EmptyState } from '~/shared/components/EmptyState/EmptyState';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';
import { toAge } from '~/shared/utils/formatters';

import { NodeIcon } from '../../../components/NodeIcon/NodeIcon';
import { UsageMeter } from '../../../components/UsageMeter/UsageMeter';
import { hostShellRowLabel } from '../../../constants';
import type { NodeRow } from '../../../types/nodes';
import { toCordonLabel, toPodsText } from '../../../utils/formatters';
import { hasHostShell, hasMeter } from '../../../utils/predicates';
import { statusOf, toneOf } from '../../../utils/selectors';
import styles from './NodeTable.module.scss';
import { VersionCell } from './VersionCell';

type Props = {
  rows: NodeRow[];
  readAt: number;
  selectedName: string | undefined;
  message: string | undefined;
  isLoading: boolean;
  canWrite: boolean;
  isNodeShellOffered: boolean;
  onSelected: (name: string) => void;
  onHostShellRequested: (nodeName: string) => void;
  onCordonToggled: (row: NodeRow) => void;
};

export function NodeTable({
  rows,
  readAt,
  selectedName,
  message,
  isLoading,
  canWrite,
  isNodeShellOffered,
  onSelected,
  onHostShellRequested,
  onCordonToggled,
}: Props) {
  const columns: DataTableColumns<NodeRow> = [
    {
      id: 'name',
      header: 'Node',
      meta: { isRowHeader: true },
      cell: ({ row }) => (
        <span className={styles.identity}>
          <NodeIcon tone={toneOf(row.original)} />
          {row.original.name}
        </span>
      ),
    },
    { id: 'role', header: 'Role', accessorKey: 'role' },
    {
      id: 'status',
      header: 'Status',
      cell: ({ row }) => (
        <ToneBadge tone={toneOf(row.original)}>{statusOf(row.original)}</ToneBadge>
      ),
    },
    { id: 'zone', header: 'Zone', accessorKey: 'zone' },
    {
      id: 'cpu',
      header: 'CPU',
      cell: ({ row }) => (
        <UsageMeter
          name={`CPU usage of ${row.original.name}`}
          percent={row.original.cpuPercent}
          isShown={hasMeter(row.original, row.original.cpuPercent)}
        />
      ),
    },
    {
      id: 'memory',
      header: 'Memory',
      cell: ({ row }) => (
        <UsageMeter
          name={`Memory usage of ${row.original.name}`}
          percent={row.original.memoryPercent}
          isShown={hasMeter(row.original, row.original.memoryPercent)}
        />
      ),
    },
    { id: 'pods', header: 'Pods', cell: ({ row }) => toPodsText(row.original) },
    { id: 'version', header: 'Version', cell: ({ row }) => <VersionCell row={row.original} /> },
    { id: 'age', header: 'Age', cell: ({ row }) => toAge(row.original.createdAt, readAt) },
    {
      id: 'actions',
      header: () => null,
      cell: ({ row }) =>
        canWrite ? (
          <span className={styles.actions}>
            {hasHostShell(row.original, isNodeShellOffered) ? (
              <Button
                variant="ghost"
                size="sm"
                icon
                title={hostShellRowLabel}
                onClick={(event) => {
                  event.stopPropagation();
                  onHostShellRequested(row.original.name);
                }}
              >
                <LucideIconSquareTerminal size="sm" />
                <span className="sr-only">
                  {hostShellRowLabel} on {row.original.name}
                </span>
              </Button>
            ) : null}
            <Button
              variant="ghost"
              size="sm"
              icon
              title={toCordonLabel(row.original.isCordoned)}
              onClick={(event) => {
                event.stopPropagation();
                onCordonToggled(row.original);
              }}
            >
              {row.original.isCordoned ? (
                <LucideIconCircleCheck size="sm" />
              ) : (
                <LucideIconBan size="sm" />
              )}
              <span className="sr-only">
                {toCordonLabel(row.original.isCordoned)} {row.original.name}
              </span>
            </Button>
          </span>
        ) : null,
    },
  ];

  return (
    <DataTable
      aria-label="Nodes"
      data={rows}
      columns={columns}
      isLoading={isLoading}
      isSortable={false}
      isRowSelected={(row) => row.name === selectedName}
      onRowClick={(row) => {
        onSelected(row.name);
      }}
      empty={message ? <EmptyState>{message}</EmptyState> : null}
    />
  );
}
