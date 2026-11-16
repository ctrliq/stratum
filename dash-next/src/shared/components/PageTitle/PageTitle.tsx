import clsx from 'clsx';
import { type ReactNode, useContext } from 'react';

import { NestedTitleContext } from '~/shared/context/NestedTitleContext';

import styles from './PageTitle.module.scss';

type Props = {
  children: ReactNode;
  className?: string;
};

export function PageTitle({ children, className }: Props) {
  const Heading = useContext(NestedTitleContext) ? 'h2' : 'h1';
  return <Heading className={clsx(styles.root, className)}>{children}</Heading>;
}
