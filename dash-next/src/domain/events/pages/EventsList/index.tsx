import { Alert, ButtonBar, ButtonBarItem } from '@ctrliq/quantic-react';
import { useContext, useState } from 'react';

import { useListCoreV1EventForAllNamespaces } from '~/api/k8s/k8sComponents';
import { isDashboardServer } from '~/domain/auth/utils/predicates';
import { toListMessage, toStaleNotice } from '~/domain/resources/utils/formatters';
import { DataTable, type DataTableColumns } from '~/shared/components/DataTable/DataTable';
import { EmptyState } from '~/shared/components/EmptyState/EmptyState';
import { PageBleed } from '~/shared/components/PageBleed/PageBleed';
import { PageTitle } from '~/shared/components/PageTitle/PageTitle';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { NamespaceContext } from '~/shared/context/NamespaceContext';
import { hasAnswered, toLoadState } from '~/shared/query/loadState';
import { polledWhen } from '~/shared/query/polling';

import { eventKind, typeFilters } from '../../constants';
import type { EventGroup, EventRow, TypeFilter } from '../../types';
import { inEventNamespace, ofType, recentWarningsIn } from '../../utils/filters';
import {
  toCountText,
  toEventsMessage,
  toObjectCount,
  toRecentWarningCount,
  toRepeatText,
} from '../../utils/formatters';
import { isWarning } from '../../utils/predicates';
import { toEventGroups, toEventRows } from '../../utils/transforms';
import styles from './EventsList.module.scss';

const toTypeColumn = <TRow extends Pick<EventRow, 'type'>>(): DataTableColumns<TRow>[number] => ({
  id: 'type',
  header: 'Type',
  cell: ({ row }) => (
    <ToneBadge tone={isWarning(row.original) ? 'warning' : 'neutral'}>
      {row.original.type}
    </ToneBadge>
  ),
});

const eventColumns: DataTableColumns<EventRow> = [
  toTypeColumn(),
  { id: 'reason', header: 'Reason', accessorKey: 'reason', meta: { isRowHeader: true } },
  { id: 'object', header: 'Object', accessorKey: 'object' },
  { id: 'message', header: 'Message', accessorKey: 'message' },
  { id: 'count', header: 'Count', cell: ({ row }) => toCountText(row.original) },
  { id: 'lastSeen', header: 'Last seen', accessorKey: 'age' },
];

const groupColumns: DataTableColumns<EventGroup> = [
  toTypeColumn(),
  { id: 'reason', header: 'Reason', accessorKey: 'reason', meta: { isRowHeader: true } },
  { id: 'objects', header: 'Objects', cell: ({ row }) => toObjectCount(row.original) },
  { id: 'count', header: 'Count', cell: ({ row }) => toRepeatText(row.original.count) },
  { id: 'lastSeen', header: 'Last seen', accessorKey: 'age' },
];

const noItems: never[] = [];

function EventsListIn() {
  const namespace = useContext(NamespaceContext);
  const isHub = !isDashboardServer(useServerConfig());
  const [typeFilter, setTypeFilter] = useState<TypeFilter>('All');
  const [isGrouped, setIsGrouped] = useState(false);
  const list = useListCoreV1EventForAllNamespaces({}, polledWhen(useContext(LiveRefreshContext)));
  const loaded = toEventRows(
    inEventNamespace(list.data?.items ?? noItems, namespace),
    list.dataUpdatedAt,
  );
  const rows = ofType(loaded, typeFilter);
  const state = toLoadState(list);
  const notice = toStaleNotice(state, hasAnswered(list));
  const message = toEventsMessage(
    toListMessage(state, hasAnswered(list), rows.length, eventKind),
    list.data?.items.length ?? 0,
  );
  const empty = message ? <EmptyState>{message}</EmptyState> : null;
  const toolbar = (
    <>
      <ButtonBar aria-label="Event type">
        {typeFilters.map((filter) => (
          <ButtonBarItem
            key={filter}
            active={filter === typeFilter}
            onClick={() => {
              setTypeFilter(filter);
            }}
          >
            {filter}
          </ButtonBarItem>
        ))}
      </ButtonBar>
      {isHub ? (
        <ButtonBar slot="end" aria-label="Grouping">
          <ButtonBarItem
            active={isGrouped}
            onClick={() => {
              setIsGrouped((isGroupedNow) => !isGroupedNow);
            }}
          >
            Group by reason
          </ButtonBarItem>
        </ButtonBar>
      ) : null}
    </>
  );

  return (
    <section className={styles.main} aria-label="Events">
      <header>
        <PageTitle className={styles.title}>Events</PageTitle>
        <p className={styles.summary}>
          {toRecentWarningCount(recentWarningsIn(loaded, list.dataUpdatedAt).length)}
        </p>
      </header>
      {notice ? <Alert type="warning">{notice}</Alert> : null}
      <PageBleed>
        {isGrouped ? (
          <DataTable
            aria-label="Events by reason"
            data={toEventGroups(rows)}
            columns={groupColumns}
            isLoading={state === 'loading'}
            isSortable={false}
            toolbar={toolbar}
            empty={empty}
          />
        ) : (
          <DataTable
            aria-label="Events"
            data={rows}
            columns={eventColumns}
            isLoading={state === 'loading'}
            isSortable={false}
            toolbar={toolbar}
            empty={empty}
          />
        )}
      </PageBleed>
    </section>
  );
}

export function EventsList() {
  const clusterId = useContext(ClusterContext);
  return <EventsListIn key={clusterId ?? ''} />;
}
