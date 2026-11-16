import { LucideIconServer } from '@ctrliq/quantic-react';
import clsx from 'clsx';

import type { Tone } from '~/shared/types/tone';

import styles from './NodeIcon.module.scss';

type Props = {
  tone: Tone;
};

export function NodeIcon({ tone }: Props) {
  return (
    <LucideIconServer
      size="sm"
      aria-hidden="true"
      className={clsx(styles.main, {
        [styles.toneSuccess]: tone === 'success',
        [styles.toneWarning]: tone === 'warning',
        [styles.toneError]: tone === 'error',
      })}
    />
  );
}
