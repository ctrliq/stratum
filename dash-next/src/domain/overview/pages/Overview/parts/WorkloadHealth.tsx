import { Panel, PanelHeader, Progress } from '@ctrliq/quantic-react';

import { badgeColorsByTone } from '~/domain/nodes/constants';

import type { HealthGroup } from '../../../types';
import styles from './WorkloadHealth.module.scss';

type Props = {
  groups: HealthGroup[];
};

export function WorkloadHealth({ groups }: Props) {
  return (
    <Panel>
      <PanelHeader slot="header">Workload Health</PanelHeader>
      <dl className={styles.groups}>
        {groups.map(({ label, count, percent, tone }) => (
          <div key={label} className={styles.group}>
            <dt>{label}</dt>
            <dd>{count}</dd>
            <Progress
              className={styles.share}
              value={percent}
              color={badgeColorsByTone[tone]}
              aria-label={`${label} share of workloads`}
            />
          </div>
        ))}
      </dl>
    </Panel>
  );
}
