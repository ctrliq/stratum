import { Alert, BadgeGroup, Panel, PanelHeader } from '@ctrliq/quantic-react';
import { useState } from 'react';

import { DataTable, type DataTableColumns } from '~/shared/components/DataTable/DataTable';
import { EmptyState } from '~/shared/components/EmptyState/EmptyState';
import { Hinted } from '~/shared/components/Hinted/Hinted';
import { toDisplayText } from '~/shared/utils/formatters';

import { noVersionEvidence, partialDetail, partialSnapshot } from '../../../../constants';
import type { NodeRow } from '../../../../types/nodes';
import type { ReachableVersionView, VersionRow } from '../../../../types/versions';
import {
  toIdentityText,
  toIdentityTitle,
  toRollupLine,
  toServingText,
  toServingTitle,
} from '../../../../utils/formatters';
import { versionRowKeyed } from '../../../../utils/selectors';
import { DriftBadge } from '../DriftBadge';
import { VerdictCell } from './VerdictCell';
import { VersionDetail } from './VersionDetail';

type Props = {
  view: ReachableVersionView;
  driftedRows: NodeRow[];
};

const columns: DataTableColumns<VersionRow> = [
  { id: 'node', header: 'Node', cell: ({ row }) => toDisplayText(row.original.nodeUID) },
  {
    id: 'component',
    header: 'Component',
    cell: ({ row }) => toDisplayText(row.original.component),
  },
  {
    id: 'expected',
    header: 'Expected',
    cell: ({ row }) => (
      <Hinted hint={toIdentityTitle(row.original.expected, 'expected')}>
        <span>{toIdentityText(row.original.expected)}</span>
      </Hinted>
    ),
  },
  {
    id: 'installed',
    header: 'Installed',
    cell: ({ row }) => (
      <Hinted hint={toIdentityTitle(row.original.installed, 'installed')}>
        <span>{toIdentityText(row.original.installed)}</span>
      </Hinted>
    ),
  },
  {
    id: 'serving',
    header: 'Serving',
    cell: ({ row }) => (
      <Hinted hint={toServingTitle(row.original)}>
        <span>{toServingText(row.original)}</span>
      </Hinted>
    ),
  },
  { id: 'verdict', header: 'Verdict', cell: ({ row }) => <VerdictCell row={row.original} /> },
];

export function VersionComparison({ view, driftedRows }: Props) {
  const [openKey, setOpenKey] = useState<string>();
  const opened = versionRowKeyed(view.rows, openKey);

  if (openKey !== undefined && opened === undefined) {
    setOpenKey(undefined);
  }

  return (
    <Panel>
      <PanelHeader slot="header">
        Version comparison
        <span slot="subtitle">{toRollupLine(view)}</span>
        {driftedRows.length > 0 ? (
          <BadgeGroup slot="meta">
            {driftedRows.map((row) => (
              <DriftBadge key={row.name} row={row} />
            ))}
          </BadgeGroup>
        ) : null}
      </PanelHeader>
      {view.isPartial ? <Alert type="warning">{partialSnapshot}</Alert> : null}
      {view.isTruncated ? <Alert type="warning">{partialDetail}</Alert> : null}
      <DataTable
        aria-label="Version comparison"
        data={view.rows}
        columns={columns}
        isSortable={false}
        isRowSelected={(row) => row.key === openKey}
        onRowClick={(row) => {
          setOpenKey(row.key);
        }}
        empty={<EmptyState>{noVersionEvidence}</EmptyState>}
      />
      {opened ? (
        <VersionDetail
          row={opened}
          reportGeneration={view.reportGeneration}
          onClosed={() => {
            setOpenKey(undefined);
          }}
        />
      ) : null}
    </Panel>
  );
}
