// eslint-disable-next-line quantic/no-experimental-api -- the detail pane is the documented use of LayoutPane; Fuzzball's Dock ships on it.
import { LayoutPane } from '@ctrliq/quantic-react';
import { type ReactNode, useContext, useEffect, useState } from 'react';
import { createPortal } from 'react-dom';

import { PaneHostContext } from '~/shared/context/PaneHostContext';

import styles from './ResourcePanel.module.scss';

type Props = {
  title: string;
  subtitle?: string;
  footer: ReactNode;
  children: ReactNode;
  onClosed: () => void;
};

const widthShare = { size: 34, minimum: 26, maximum: 66 };

const keepsEscapeInForm = (event: KeyboardEvent): void => {
  if (
    event.key === 'Escape' &&
    event.target instanceof Element &&
    event.target.closest('form') !== null
  ) {
    event.preventDefault();
  }
};

export function ResourcePanel({ title, subtitle, footer, children, onClosed }: Props) {
  const host = useContext(PaneHostContext);
  const [body, setBody] = useState<HTMLDivElement | null>(null);

  useEffect(() => {
    body?.addEventListener('keydown', keepsEscapeInForm);
    return () => {
      body?.removeEventListener('keydown', keepsEscapeInForm);
    };
  }, [body]);

  const pane = (
    <LayoutPane
      slot="pane"
      aria-label={title}
      open
      resizable
      size={widthShare.size}
      minSize={widthShare.minimum}
      maxSize={widthShare.maximum}
      onOpenChanged={(event) => {
        if (event.target === event.currentTarget && !event.detail.open) {
          onClosed();
        }
      }}
    >
      <h2 slot="toolbar" className={styles.title}>
        {title}
      </h2>
      <div ref={setBody} className={styles.content}>
        {subtitle ? <p className={styles.subtitle}>{subtitle}</p> : null}
        {children}
      </div>
      {footer ? (
        <footer slot="footer" className={styles.actions}>
          {footer}
        </footer>
      ) : null}
    </LayoutPane>
  );

  return host === null ? pane : createPortal(pane, host);
}
