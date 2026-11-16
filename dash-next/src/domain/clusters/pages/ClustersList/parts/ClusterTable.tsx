import { type ReactNode, useMemo } from 'react';

import type { Cluster } from '~/api/fleet/fleetSchemas';
import { DataTable, type DataTableColumns } from '~/shared/components/DataTable/DataTable';
import { toDisplayText } from '~/shared/utils/formatters';

import { toContext, toNodesText, toProviderText, toVersionText } from '../../../utils/formatters';
import { ClusterName, ClusterStatus, EngineStateLink, UseCluster } from './ClusterEntry';

type Props = {
  clusters: Cluster[];
  actionsFor: (cluster: Cluster) => ReactNode;
};

export const ClusterTable = ({ clusters, actionsFor }: Props) => {
  const columns = useMemo<DataTableColumns<Cluster>>(
    () => [
      {
        id: 'cluster',
        header: 'Cluster',
        meta: { isRowHeader: true },
        cell: ({ row }) => <ClusterName cluster={row.original} />,
      },
      { id: 'context', header: 'Context', accessorFn: toContext },
      {
        id: 'status',
        header: 'Status',
        cell: ({ row }) => <ClusterStatus cluster={row.original} />,
      },
      { id: 'provider', header: 'Provider', accessorFn: toProviderText },
      { id: 'region', header: 'Region', accessorFn: ({ region }) => toDisplayText(region) },
      {
        id: 'version',
        header: 'Version',
        accessorFn: ({ k8sVersion }) => toVersionText(k8sVersion),
      },
      { id: 'nodes', header: 'Nodes', accessorFn: toNodesText },
      {
        id: 'current',
        header: 'Current',
        cell: ({ row }) => <UseCluster cluster={row.original} />,
      },
      {
        id: 'engineState',
        header: 'Engine state',
        cell: ({ row }) => <EngineStateLink cluster={row.original} />,
      },
      { id: 'actions', header: '', cell: ({ row }) => actionsFor(row.original) },
    ],
    [actionsFor],
  );
  return <DataTable data={clusters} columns={columns} isSortable={false} empty={null} />;
};
