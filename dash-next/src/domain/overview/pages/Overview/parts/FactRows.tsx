import clsx from 'clsx';
import type { ReactNode } from 'react';

import styles from './FactRows.module.scss';

type Props = {
  children: ReactNode;
};

type FactProps = {
  label: string;
  isWarning?: boolean;
  children: ReactNode;
};

export function FactRows({ children }: Props) {
  return <dl className={styles.main}>{children}</dl>;
}

export function Fact({ label, isWarning = false, children }: FactProps) {
  return (
    <div className={styles.fact}>
      <dt>{label}</dt>
      <dd className={clsx({ [styles.stateWarning]: isWarning })}>{children}</dd>
    </div>
  );
}
