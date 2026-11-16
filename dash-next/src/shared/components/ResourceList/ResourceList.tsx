import { ActionGroup, Alert, Button, LucideIconSearch, TextInput } from '@ctrliq/quantic-react';
import type { MouseEvent, ReactNode } from 'react';

import { DataTable, type DataTableColumns } from '~/shared/components/DataTable/DataTable';
import { EmptyState } from '~/shared/components/EmptyState/EmptyState';
import { PageBleed } from '~/shared/components/PageBleed/PageBleed';
import { PageTitle } from '~/shared/components/PageTitle/PageTitle';

import styles from './ResourceList.module.scss';

export type RowAction<TRow> = {
  title: string;
  icon: ReactNode;
  onRun: (row: TRow) => void;
};

type Props<TRow> = {
  title: string;
  tags?: ReactNode;
  summary: string;
  rows: TRow[];
  columns: DataTableColumns<TRow>;
  nameOf: (row: TRow) => string;
  openPrefix: string;
  isRowSelected: (row: TRow) => boolean;
  onRowOpened: (row: TRow) => void;
  rowActions?: RowAction<TRow>[];
  isLoading: boolean;
  message?: string;
  notice?: string;
  search: string;
  searchName: string;
  searchPlaceholder: string;
  onSearchChanged: (search: string) => void;
  primaryAction?: ReactNode;
  panel?: ReactNode;
};

const withoutRowOpening =
  (onRun: () => void) =>
  (event: MouseEvent): void => {
    event.stopPropagation();
    onRun();
  };

export function ResourceList<TRow>({
  title,
  tags,
  summary,
  rows,
  columns,
  nameOf,
  openPrefix,
  isRowSelected,
  onRowOpened,
  rowActions = [],
  isLoading,
  message,
  notice,
  search,
  searchName,
  searchPlaceholder,
  onSearchChanged,
  primaryAction,
  panel,
}: Props<TRow>) {
  const nameColumn: DataTableColumns<TRow>[number] = {
    id: 'name',
    header: 'Name',
    meta: { isRowHeader: true },
    cell: ({ row }) => (
      <Button
        className={styles.openControl}
        variant="ghost"
        size="sm"
        aria-current={isRowSelected(row.original) ? 'true' : undefined}
      >
        <span className="sr-only">{openPrefix} </span>
        {nameOf(row.original)}
      </Button>
    ),
  };

  const actionColumn: DataTableColumns<TRow>[number] = {
    id: 'actions',
    header: () => null,
    cell: ({ row }) => (
      <ActionGroup aria-label={`Actions for ${nameOf(row.original)}`}>
        {rowActions.map((action) => (
          <Button
            key={action.title}
            variant="ghost"
            size="sm"
            icon
            title={action.title}
            onClick={withoutRowOpening(() => action.onRun(row.original))}
          >
            {action.icon}
            <span className="sr-only">
              {action.title} {nameOf(row.original)}
            </span>
          </Button>
        ))}
      </ActionGroup>
    ),
  };

  return (
    <section className={styles.main} aria-label={title}>
      <header className={styles.heading}>
        <PageTitle className={styles.title}>{title}</PageTitle>
        {tags}
        <p className={styles.summary}>{summary}</p>
      </header>
      {notice ? <Alert type="warning">{notice}</Alert> : null}
      <PageBleed className={styles.content}>
        <DataTable
          aria-label={title}
          data={rows}
          columns={[nameColumn, ...columns, ...(rowActions.length > 0 ? [actionColumn] : [])]}
          isLoading={isLoading}
          isSortable={false}
          isRowSelected={isRowSelected}
          onRowClick={onRowOpened}
          empty={message ? <EmptyState>{message}</EmptyState> : null}
          toolbar={
            <>
              <TextInput
                className={styles.search}
                type="search"
                aria-label={searchName}
                placeholder={searchPlaceholder}
                value={search}
                onValueChanged={(event) => onSearchChanged(event.detail.value)}
              >
                <LucideIconSearch slot="prefix" size="sm" />
              </TextInput>
              {primaryAction ? <span slot="end">{primaryAction}</span> : null}
            </>
          }
        />
      </PageBleed>
      {panel}
    </section>
  );
}
