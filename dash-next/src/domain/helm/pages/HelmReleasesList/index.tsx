import { CellAlign } from '@ctrliq/quantic-components';
import { Alert } from '@ctrliq/quantic-react';
import { useContext } from 'react';

import { toListMessage, toStaleNotice } from '~/domain/resources/utils/formatters';
import { DataTable, type DataTableColumns } from '~/shared/components/DataTable/DataTable';
import { EmptyState } from '~/shared/components/EmptyState/EmptyState';
import { PageBleed } from '~/shared/components/PageBleed/PageBleed';
import { PageTitle } from '~/shared/components/PageTitle/PageTitle';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';
import { NamespaceContext } from '~/shared/context/NamespaceContext';
import { inNamespace } from '~/shared/utils/filters';

import { releaseKind } from '../../constants';
import { useReleaseSecrets } from '../../hooks/useReleaseSecrets';
import type { HelmRelease } from '../../types';
import { toReleaseSummary, toStatusLabel, toStatusTone } from '../../utils/formatters';
import { toHelmReleases } from '../../utils/transforms';
import styles from './HelmReleasesList.module.scss';

const columns: DataTableColumns<HelmRelease> = [
  { id: 'name', header: 'Release', accessorKey: 'name', meta: { isRowHeader: true } },
  { id: 'namespace', header: 'Namespace', accessorKey: 'namespace' },
  { id: 'chart', header: 'Chart', accessorKey: 'chart' },
  { id: 'appVersion', header: 'App version', accessorKey: 'appVersion' },
  {
    id: 'status',
    header: 'Status',
    cell: ({ row }) => (
      <ToneBadge tone={toStatusTone(row.original.status)}>
        {toStatusLabel(row.original.status)}
      </ToneBadge>
    ),
  },
  { id: 'revision', header: 'Rev', accessorKey: 'revision', meta: { align: CellAlign.Right } },
  { id: 'updated', header: 'Updated', accessorKey: 'updated' },
];

const noSecrets: never[] = [];

export function HelmReleasesList() {
  const namespace = useContext(NamespaceContext);
  const { secrets, state, readAt } = useReleaseSecrets();
  const releases = toHelmReleases(inNamespace(secrets ?? noSecrets, namespace), readAt);
  const hasAnswered = secrets !== undefined;
  const notice = toStaleNotice(state, hasAnswered);
  const message = toListMessage(state, hasAnswered, releases.length, releaseKind);

  return (
    <section className={styles.main} aria-label="Helm Releases">
      <header>
        <PageTitle className={styles.title}>Helm Releases</PageTitle>
        <p className={styles.summary}>{toReleaseSummary(releases.length)}</p>
      </header>
      {notice ? <Alert type="warning">{notice}</Alert> : null}
      <PageBleed>
        <DataTable
          aria-label="Helm releases"
          data={releases}
          columns={columns}
          isLoading={state === 'loading'}
          isSortable={false}
          empty={message ? <EmptyState>{message}</EmptyState> : null}
        />
      </PageBleed>
    </section>
  );
}
