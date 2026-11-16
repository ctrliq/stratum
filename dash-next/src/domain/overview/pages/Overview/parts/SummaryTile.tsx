import { Panel, Progress } from '@ctrliq/quantic-react';

import { badgeColorsByTone } from '~/domain/nodes/constants';
import { toUsageTone } from '~/shared/utils/formatters';

import styles from './SummaryTile.module.scss';

type Props = {
  label: string;
  value: string;
  caption?: string;
  percent?: number;
};

export function SummaryTile({ label, value, caption, percent }: Props) {
  return (
    <Panel>
      <h2 className={styles.label}>{label}</h2>
      <p className={styles.value}>{value}</p>
      {percent === undefined ? null : (
        <Progress
          value={percent}
          color={badgeColorsByTone[toUsageTone(percent) ?? 'success']}
          aria-label={label}
        />
      )}
      {caption ? <p className={styles.caption}>{caption}</p> : null}
    </Panel>
  );
}
