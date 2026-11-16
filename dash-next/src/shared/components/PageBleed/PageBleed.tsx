import clsx from 'clsx';
import type { ReactNode } from 'react';

import styles from './PageBleed.module.scss';

type Props = {
  children: ReactNode;
  className?: string;
};

export function PageBleed({ children, className }: Props) {
  return <div className={clsx(styles.root, className)}>{children}</div>;
}
