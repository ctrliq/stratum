import { Button, Card, CardTitle } from '@ctrliq/quantic-react';
import type { MouseEvent, ReactNode } from 'react';

import { AppMark } from '~/domain/catalog/components/AppMark';
import type { CatalogApp } from '~/domain/catalog/types/catalog';

import styles from './AppCard.module.scss';

type Props = {
  app: CatalogApp;
  subtitle: string;
  isSelected: boolean;
  footer: ReactNode;
  onOpened: (app: CatalogApp) => void;
};

export const AppCard = ({ app, subtitle, isSelected, footer, onOpened }: Props) => (
  <Card
    selected={isSelected}
    onClick={() => {
      onOpened(app);
    }}
  >
    <CardTitle slot="header">
      <span className={styles.heading}>
        <AppMark app={app} />
        <Button
          variant="ghost"
          size="sm"
          aria-current={isSelected ? 'true' : undefined}
          onClick={(event: MouseEvent) => {
            event.stopPropagation();
            onOpened(app);
          }}
        >
          {app.name}
        </Button>
      </span>
      <span slot="subtitle">{subtitle}</span>
    </CardTitle>
    {app.shortDescription}
    <footer slot="footer" className={styles.footing}>
      {footer}
    </footer>
  </Card>
);
