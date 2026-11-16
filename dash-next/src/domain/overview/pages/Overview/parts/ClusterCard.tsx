import { Panel, PanelHeader } from '@ctrliq/quantic-react';
import type { MouseEvent } from 'react';

import type { FactRow } from '../../../types';
import { Fact, FactRows } from './FactRows';

type Props = {
  rows: FactRow[];
  defaultStorageClass: string;
  storageHref: string;
  hubRows: FactRow[];
  onLinkClicked: (event: MouseEvent, href: string) => void;
};

export function ClusterCard({
  rows,
  defaultStorageClass,
  storageHref,
  hubRows,
  onLinkClicked,
}: Props) {
  return (
    <Panel>
      <PanelHeader slot="header">Cluster</PanelHeader>
      <FactRows>
        {rows.map(({ label, value }) => (
          <Fact key={label} label={label}>
            {value}
          </Fact>
        ))}
        <Fact label="Default storage">
          <a
            href={storageHref}
            onClick={(event) => {
              onLinkClicked(event, storageHref);
            }}
          >
            {defaultStorageClass}
          </a>
        </Fact>
        {hubRows.map(({ label, value, isWarning }) => (
          <Fact key={label} label={label} isWarning={isWarning}>
            {value}
          </Fact>
        ))}
      </FactRows>
    </Panel>
  );
}
