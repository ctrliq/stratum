import { Panel, PanelHeader } from '@ctrliq/quantic-react';

import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';

import type { ControlPlaneSummary, FactRow } from '../../../types';
import styles from './ControlPlaneCard.module.scss';
import { Fact, FactRows } from './FactRows';

type Props = {
  summary: ControlPlaneSummary;
  rows: FactRow[];
};

export function ControlPlaneCard({ summary, rows }: Props) {
  return (
    <Panel>
      <PanelHeader slot="header">
        Control Plane
        <span slot="subtitle">{summary.caption}</span>
        <ToneBadge slot="meta" tone={summary.tone}>
          {summary.badge}
        </ToneBadge>
      </PanelHeader>
      <p className={styles.figure}>{summary.value}</p>
      <FactRows>
        {rows.map(({ label, value, isWarning }) => (
          <Fact key={label} label={label} isWarning={isWarning}>
            {value}
          </Fact>
        ))}
      </FactRows>
    </Panel>
  );
}
