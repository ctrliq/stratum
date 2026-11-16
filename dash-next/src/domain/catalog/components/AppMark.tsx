import { LucideIconBox } from '@ctrliq/quantic-react';
import type { CSSProperties } from 'react';

import type { CatalogApp } from '../types/catalog';
import styles from './AppMark.module.scss';

type Props = {
  app: Pick<CatalogApp, 'logo' | 'accent'>;
};

export const AppMark = ({ app }: Props) =>
  app.logo ? (
    <img className={styles.main} src={app.logo} alt="" />
  ) : (
    <span
      className={styles.main}
      style={app.accent ? ({ '--accent': app.accent } as CSSProperties) : undefined}
      aria-hidden="true"
    >
      <LucideIconBox size="md" />
    </span>
  );
