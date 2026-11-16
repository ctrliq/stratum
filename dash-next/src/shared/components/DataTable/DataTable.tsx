import { CellAlign, SortDirection, TableSize } from '@ctrliq/quantic-components';
import {
  Table,
  TableBody,
  TableCell,
  TableControls,
  TableHead,
  TableHeader,
  TableRow,
} from '@ctrliq/quantic-react';
import {
  type ColumnDef,
  flexRender,
  getCoreRowModel,
  getSortedRowModel,
  type RowData,
  type SortingState,
  useReactTable,
} from '@tanstack/react-table';
import clsx from 'clsx';
import { type ComponentPropsWithoutRef, type ReactNode, useState } from 'react';

import { EmptyState } from '~/shared/components/EmptyState/EmptyState';

import styles from './DataTable.module.scss';
import { SkeletonRows } from './SkeletonRows';

declare module '@tanstack/react-table' {
  interface ColumnMeta<TData extends RowData, TValue> {
    align?: CellAlign;
    isRowHeader?: boolean;
  }
}

type Props<TData> = {
  data: TData[];
  // eslint-disable-next-line @typescript-eslint/no-explicit-any -- columns may each have a different TValue, so the array type stays open.
  columns: ColumnDef<TData, any>[];
  toolbar?: ReactNode;
  footer?: ReactNode;
  empty?: ReactNode;
  isLoading?: boolean;
  isSortable?: boolean;
  isRowSelected?: (row: TData) => boolean;
  onRowClick?: (row: TData) => void;
} & ComponentPropsWithoutRef<typeof Table>;

export type DataTableColumns<TData> = Props<TData>['columns'];

const toQuanticSortDirection = (columnId: string, sorting: SortingState): SortDirection => {
  const match = sorting.find((sort) => sort.id === columnId);
  return match === undefined
    ? SortDirection.None
    : match.desc
      ? SortDirection.Descending
      : SortDirection.Ascending;
};

const defaultEmpty = <EmptyState title="No results">No data available.</EmptyState>;

export function DataTable<TData>({
  className,
  data,
  columns,
  toolbar,
  footer,
  empty = defaultEmpty,
  isLoading = false,
  isSortable = true,
  isRowSelected,
  onRowClick,
  ...props
}: Props<TData>) {
  const [sorting, setSorting] = useState<SortingState>([]);

  const table = useReactTable({
    data,
    columns,
    state: { sorting },
    enableSorting: isSortable,
    onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
  });

  const rows = table.getRowModel().rows;

  return (
    <>
      <Table size={TableSize.Small} className={clsx(styles.main, className)} {...props}>
        {toolbar ? <TableControls slot="toolbar">{toolbar}</TableControls> : null}
        <TableHeader>
          {table.getHeaderGroups().map((headerGroup) => (
            <TableRow key={headerGroup.id}>
              {headerGroup.headers.map((header) => (
                <TableHead
                  key={header.id}
                  sortable={header.column.getCanSort()}
                  sortDirection={toQuanticSortDirection(header.column.id, sorting)}
                  sortKey={header.column.id}
                  align={header.column.columnDef.meta?.align ?? CellAlign.Left}
                  onSortChange={(event) =>
                    setSorting([{ id: header.column.id, desc: event.detail.direction === 'desc' }])
                  }
                >
                  {header.isPlaceholder
                    ? null
                    : flexRender(header.column.columnDef.header, header.getContext())}
                </TableHead>
              ))}
            </TableRow>
          ))}
        </TableHeader>

        {isLoading ? (
          <SkeletonRows columnCount={columns.length} />
        ) : (
          <TableBody>
            {rows.map((row) => (
              <TableRow
                key={row.id}
                selected={isRowSelected?.(row.original) ?? false}
                className={clsx({ [styles.stateOpenable]: onRowClick !== undefined })}
                onClick={() => onRowClick?.(row.original)}
              >
                {row.getVisibleCells().map((cell) => (
                  <TableCell
                    key={cell.id}
                    role={cell.column.columnDef.meta?.isRowHeader ? 'rowheader' : 'cell'}
                    align={cell.column.columnDef.meta?.align ?? CellAlign.Left}
                  >
                    {flexRender(cell.column.columnDef.cell, cell.getContext())}
                  </TableCell>
                ))}
              </TableRow>
            ))}
          </TableBody>
        )}
        {footer && rows.length > 0 ? <TableControls slot="controls">{footer}</TableControls> : null}
      </Table>

      {!isLoading && rows.length === 0 ? empty : null}
    </>
  );
}
