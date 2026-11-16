import { Button } from '@ctrliq/quantic-react';

import { AppMark } from '~/domain/catalog/components/AppMark';
import { ReleaseStatus } from '~/domain/catalog/components/ReleaseStatus';
import type { AppListing, CatalogApp } from '~/domain/catalog/types/catalog';
import { toStatusText } from '~/domain/catalog/utils/formatters';
import { releaseStateOf } from '~/domain/catalog/utils/selectors';
import { DataTable, type DataTableColumns } from '~/shared/components/DataTable/DataTable';
import { absentText } from '~/shared/utils/formatters';

import styles from './AppsTable.module.scss';
import { QuickInstall } from './QuickInstall';

export const AppsTable = ({
  apps,
  releases,
  now,
  selectedId,
  canInstall,
  isBusy,
  onOpened,
  onQuickInstall,
}: AppListing) => {
  const columns: DataTableColumns<CatalogApp> = [
    {
      id: 'application',
      header: 'Application',
      meta: { isRowHeader: true },
      cell: ({ row }) => (
        <span className={styles.application}>
          <AppMark app={row.original} />
          <span className={styles.identity}>
            <Button
              className={styles.openControl}
              variant="ghost"
              size="sm"
              aria-current={row.original.id === selectedId ? 'true' : undefined}
            >
              {row.original.name}
            </Button>
            <span className={styles.description}>{row.original.shortDescription}</span>
          </span>
        </span>
      ),
    },
    { id: 'version', header: 'Version', accessorFn: ({ chart }) => chart.version },
    { id: 'category', header: 'Category', accessorKey: 'category' },
    {
      id: 'status',
      header: 'Status',
      cell: ({ row }) => {
        const chart = releases.get(row.original.id);
        return chart === undefined ? (
          absentText
        ) : (
          <ReleaseStatus
            state={releaseStateOf(chart, now)}
            text={toStatusText(releaseStateOf(chart, now), chart, now)}
          />
        );
      },
    },
    {
      id: 'action',
      header: 'Action',
      cell: ({ row }) =>
        canInstall && !releases.has(row.original.id) ? (
          <QuickInstall
            isBusy={isBusy}
            onPressed={() => {
              onQuickInstall(row.original);
            }}
          />
        ) : null,
    },
  ];
  return (
    <DataTable
      aria-label="Applications"
      data={apps}
      columns={columns}
      isSortable={false}
      isRowSelected={(app) => app.id === selectedId}
      onRowClick={onOpened}
      empty={null}
    />
  );
};
