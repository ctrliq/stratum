import type { ReactNode } from 'react';

import styles from './EngineStateFacts.module.scss';

type FactProps = {
  term: string;
  children: ReactNode;
};

type Props = {
  children: ReactNode;
};

export const EngineStateFact = ({ term, children }: FactProps) => (
  <div className={styles.fact}>
    <dt className={styles.term}>{term}</dt>
    <dd className={styles.detail}>{children}</dd>
  </div>
);

export const EngineStateFacts = ({ children }: Props) => (
  <dl className={styles.facts}>{children}</dl>
);

export const EngineStateNote = ({ children }: Props) => (
  <p role="status" className={styles.note}>
    {children}
  </p>
);
