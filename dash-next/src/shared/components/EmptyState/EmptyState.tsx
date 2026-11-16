import clsx from 'clsx';
import { type ReactNode } from 'react';

import styles from './EmptyState.module.scss';

type Props = {
  title?: ReactNode;
  children?: ReactNode;
  actions?: ReactNode;
  className?: string;
  fill?: boolean;
};

export function EmptyState({ title, children, actions, className, fill }: Props) {
  return (
    <div className={clsx(styles.main, { [styles.variantFill]: fill }, className)}>
      {title ? <p className={styles.title}>{title}</p> : null}
      {children}
      {actions ? <div className={styles.actions}>{actions}</div> : null}
    </div>
  );
}
