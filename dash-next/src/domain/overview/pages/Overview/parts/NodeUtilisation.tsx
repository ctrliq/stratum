import { CellAlign } from '@ctrliq/quantic-components';
import { Panel, PanelHeader } from '@ctrliq/quantic-react';
import type { MouseEvent } from 'react';

import { NodeIcon } from '~/domain/nodes/components/NodeIcon/NodeIcon';
import { UsageMeter } from '~/domain/nodes/components/UsageMeter/UsageMeter';
import type { NodeRow } from '~/domain/nodes/types/nodes';
import { hasMeter } from '~/domain/nodes/utils/predicates';
import { DataTable, type DataTableColumns } from '~/shared/components/DataTable/DataTable';
import { Hinted } from '~/shared/components/Hinted/Hinted';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';

import { readinessTones } from '../../../constants';
import { toNodeLinkTitle, toShortRole } from '../../../utils/formatters';
import { withNodeSelected } from '../../../utils/transforms';
import styles from './NodeUtilisation.module.scss';

type Props = {
  rows: NodeRow[];
  nodesHref: string;
  onLinkClicked: (event: MouseEvent, href: string) => void;
};

export function NodeUtilisation({ rows, nodesHref, onLinkClicked }: Props) {
  const columns: DataTableColumns<NodeRow> = [
    {
      id: 'name',
      header: 'Node',
      meta: { isRowHeader: true },
      cell: ({ row }) => {
        const href = withNodeSelected(nodesHref, row.original.name);
        return (
          <span className={styles.identity}>
            <NodeIcon tone={readinessTones[row.original.readiness] ?? 'neutral'} />
            <Hinted hint={toNodeLinkTitle(row.original.name)}>
              <a
                href={href}
                onClick={(event) => {
                  onLinkClicked(event, href);
                }}
              >
                {row.original.name}
              </a>
            </Hinted>
            {row.original.isCordoned ? <ToneBadge tone="warning">Cordoned</ToneBadge> : null}
          </span>
        );
      },
    },
    { id: 'role', header: 'Role', cell: ({ row }) => toShortRole(row.original.role) },
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
    { id: 'pods', header: 'Pods', accessorKey: 'podCount', meta: { align: CellAlign.Right } },
  ];

  return (
    <Panel>
      <PanelHeader slot="header">
        Node Utilization
        <a
          slot="meta"
          href={nodesHref}
          onClick={(event) => {
            onLinkClicked(event, nodesHref);
          }}
        >
          View all
        </a>
      </PanelHeader>
      {rows.length === 0 ? (
        <p className={styles.note}>No node data yet.</p>
      ) : (
        <DataTable aria-label="Node utilization" data={rows} columns={columns} isSortable={false} />
      )}
    </Panel>
  );
}
