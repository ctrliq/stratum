import { Progress } from '@ctrliq/quantic-react';

import { absentText, toPercentText, toUsageTone } from '~/shared/utils/formatters';

import { badgeColorsByTone } from '../../constants';
import styles from './UsageMeter.module.scss';

type Props = {
  name: string;
  percent: number;
  isShown: boolean;
};

export function UsageMeter({ name, percent, isShown }: Props) {
  return isShown ? (
    <span className={styles.main}>
      <Progress
        className={styles.bar}
        value={percent}
        color={badgeColorsByTone[toUsageTone(percent) ?? 'success']}
        aria-label={name}
      />
      {toPercentText(percent)}
    </span>
  ) : (
    absentText
  );
}
