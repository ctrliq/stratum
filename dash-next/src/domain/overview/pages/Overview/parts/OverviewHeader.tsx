import { Button, LucideIconRefreshCw } from '@ctrliq/quantic-react';

import { PageTitle } from '~/shared/components/PageTitle/PageTitle';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';
import { toStatusLabel, toStatusTone } from '~/shared/utils/formatters';

import { staleFiguresNote } from '../../../constants';
import styles from './OverviewHeader.module.scss';

type Props = {
  name: string;
  status: string;
  line: string;
  isFleetManager: boolean;
  asOf: string | undefined;
  onRefreshed: () => void;
};

export function OverviewHeader({ name, status, line, isFleetManager, asOf, onRefreshed }: Props) {
  return (
    <header className={styles.main}>
      <hgroup>
        <div className={styles.identity}>
          <PageTitle className={styles.title}>{name}</PageTitle>
          <ToneBadge tone={toStatusTone(status)}>{toStatusLabel(status)}</ToneBadge>
          {isFleetManager ? <ToneBadge tone="neutral">Fleet manager</ToneBadge> : null}
          {asOf ? (
            <ToneBadge tone="warning" hint={staleFiguresNote}>
              {asOf}
            </ToneBadge>
          ) : null}
        </div>
        <p className={styles.line}>{line}</p>
      </hgroup>
      <Button variant="outline" onClick={onRefreshed}>
        <LucideIconRefreshCw size="sm" />
        Refresh
      </Button>
    </header>
  );
}
