import { CellAlign } from '@ctrliq/quantic-components';
import { Alert, Badge, LucideIconSearch, TextInput } from '@ctrliq/quantic-react';
import { useContext, useState } from 'react';

import { loadingSummary } from '~/domain/resources/constants';
import { useScreenNavigation } from '~/domain/shell/hooks/useScreenNavigation';
import { DataTable, type DataTableColumns } from '~/shared/components/DataTable/DataTable';
import { EmptyState } from '~/shared/components/EmptyState/EmptyState';
import { PageTitle } from '~/shared/components/PageTitle/PageTitle';
import { StatusCell } from '~/shared/components/ToneBadge/StatusCell';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { NamespaceContext } from '~/shared/context/NamespaceContext';
import { toDisplayText } from '~/shared/utils/formatters';

import { overviewTitle, workloadSearchName, workloadSearchPlaceholder } from '../../constants';
import { useWorkloadSnapshot } from '../../hooks/useWorkloadSnapshot';
import type { AgedWorkloadRow, WorkloadKind } from '../../types/workloads';
import { visibleUnder } from '../../utils/filters';
import {
  toCpuText,
  toDegradedFlag,
  toEmptyWorkloadsMessage,
  toMemoryText,
  toUnreadableNotice,
  toWorkloadsSummary,
} from '../../utils/formatters';
import { isFirstLoad, isWhollyRead } from '../../utils/predicates';
import { countsByKind, degradedCountOf, unreadableAmong } from '../../utils/selectors';
import { toWorkloadHref } from '../../utils/transforms';
import { KindFilter } from './parts/KindFilter';
import { NameCell } from './parts/NameCell';
import { ReadyCell } from './parts/ReadyCell';
import styles from './WorkloadsOverview.module.scss';

const numeric = { align: CellAlign.Right };

const columns: DataTableColumns<AgedWorkloadRow> = [
  { id: 'name', header: 'Name', cell: NameCell, meta: { isRowHeader: true } },
  {
    id: 'kind',
    header: 'Kind',
    cell: ({ row }) => (
      <Badge color="gray" size="sm">
        {row.original.kind}
      </Badge>
    ),
  },
  { id: 'namespace', header: 'Namespace', accessorFn: (row) => toDisplayText(row.namespace) },
  { id: 'ready', header: 'Ready', cell: ReadyCell },
  { id: 'status', header: 'Status', accessorKey: 'status', cell: StatusCell },
  { id: 'cpu', header: 'CPU', accessorFn: (row) => toCpuText(row.millicores), meta: numeric },
  { id: 'memory', header: 'Mem', accessorFn: (row) => toMemoryText(row.mebibytes), meta: numeric },
  { id: 'restarts', header: 'Restarts', accessorFn: (row) => row.restarts ?? 0, meta: numeric },
  { id: 'age', header: 'Age', accessorKey: 'age' },
];

export function WorkloadsOverview() {
  const clusterId = useContext(ClusterContext);
  const namespace = useContext(NamespaceContext);
  const { onOpened } = useScreenNavigation();
  const { rows, reads } = useWorkloadSnapshot();
  const [kind, setKind] = useState<WorkloadKind>();
  const [search, setSearch] = useState('');

  const visible = visibleUnder(rows, { namespace, kind, search });
  const isLoading = isFirstLoad(reads);
  const degraded = degradedCountOf(rows);

  return (
    <section className={styles.main} aria-label={overviewTitle}>
      <header className={styles.heading}>
        <PageTitle className={styles.title}>{overviewTitle}</PageTitle>
        <p className={styles.summary}>
          {isLoading ? loadingSummary : toWorkloadsSummary(visible.length, rows.length, namespace)}
          {degraded > 0 ? (
            <strong className={styles.degraded}>{toDegradedFlag(degraded)}</strong>
          ) : null}
        </p>
      </header>
      {unreadableAmong(reads).map((read) => (
        <Alert key={read.plural} type="warning">
          {toUnreadableNotice(read)}
        </Alert>
      ))}
      <DataTable
        aria-label={overviewTitle}
        data={visible}
        columns={columns}
        isLoading={isLoading}
        isSortable={false}
        onRowClick={(row) => onOpened(toWorkloadHref(row, clusterId, namespace))}
        empty={
          isWhollyRead(reads) ? <EmptyState>{toEmptyWorkloadsMessage(kind)}</EmptyState> : null
        }
        toolbar={
          <>
            <KindFilter
              chosen={kind}
              counts={countsByKind(rows)}
              total={rows.length}
              onChosen={setKind}
            />
            <TextInput
              className={styles.search}
              type="search"
              aria-label={workloadSearchName}
              placeholder={workloadSearchPlaceholder}
              value={search}
              onValueChanged={(event) => setSearch(event.detail.value)}
            >
              <LucideIconSearch slot="prefix" size="sm" />
            </TextInput>
          </>
        }
      />
    </section>
  );
}
