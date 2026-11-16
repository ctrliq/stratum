import { Button, LucideIconX } from '@ctrliq/quantic-react';
import type { ReactNode } from 'react';

import type { CatalogApp } from '../types/catalog';
import { toChartLine } from '../utils/formatters';
import styles from './AppDetailFrame.module.scss';
import { AppMark } from './AppMark';

type Props = {
  app: CatalogApp;
  status?: ReactNode;
  footer: ReactNode;
  children: ReactNode;
  onClosed: () => void;
};

export const AppDetailFrame = ({ app, status, footer, children, onClosed }: Props) => (
  <section className={styles.main} aria-label={app.name}>
    <header className={styles.heading}>
      <AppMark app={app} />
      <hgroup className={styles.identity}>
        <h2 className={styles.title}>{app.name}</h2>
        <p className={styles.subtitle}>
          {toChartLine(app)} · {app.category}
        </p>
        {app.home ? (
          <a className={styles.home} href={app.home} target="_blank" rel="noopener noreferrer">
            Project home ↗
          </a>
        ) : null}
      </hgroup>
      {status}
      <Button variant="ghost" size="sm" icon title="Close" onClick={onClosed}>
        <LucideIconX size="sm" />
        <span className="sr-only">Close</span>
      </Button>
    </header>
    <div className={styles.content}>{children}</div>
    <footer className={styles.actions}>{footer}</footer>
  </section>
);
