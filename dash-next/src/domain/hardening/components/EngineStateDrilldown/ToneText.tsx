import clsx from 'clsx';

import type { Tone } from '~/shared/types/tone';

import styles from './ToneText.module.scss';

type Props = {
  tone: Tone;
  children: string;
};

export const ToneText = ({ tone, children }: Props) => (
  <span
    className={clsx(styles.text, {
      [styles.toneSuccess]: tone === 'success',
      [styles.toneWarning]: tone === 'warning',
      [styles.toneError]: tone === 'error',
    })}
  >
    {children}
  </span>
);
