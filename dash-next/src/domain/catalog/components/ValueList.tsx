import { Truncate } from '@ctrliq/quantic-react';
import clsx from 'clsx';

import { toDisplayText } from '~/shared/utils/formatters';

import type { StatusTone, ValueRow } from '../types/catalog';
import styles from './ValueList.module.scss';

type Props = {
  title: string;
  rows: (ValueRow & { tone?: StatusTone })[];
};

export const ValueList = ({ title, rows }: Props) => (
  <section className={styles.main} aria-label={title}>
    <h3 className={styles.title}>{title}</h3>
    <dl className={styles.rows}>
      {rows.map(({ key, value, tone }) => (
        <div key={`${key}/${value}`} className={styles.row}>
          <dt>{key}</dt>
          <dd
            className={clsx({
              [styles.toneSuccess]: tone === 'success',
              [styles.toneError]: tone === 'error',
            })}
          >
            <Truncate value={toDisplayText(value)} />
          </dd>
        </div>
      ))}
    </dl>
  </section>
);
