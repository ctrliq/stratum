import { StatusBadge } from '@ctrliq/quantic-react';

import type { SessionStatus } from '../../types/terminal';
import { colorFor } from '../../utils/selectors';
import styles from './SessionHeader.module.scss';

type Props = {
  status: SessionStatus;
  label: string;
};

export function SessionHeader({ status, label }: Props) {
  return (
    <header className={styles.main}>
      <StatusBadge color={colorFor(status)} size="sm">
        {status}
      </StatusBadge>
      <code className={styles.label}>{label}</code>
    </header>
  );
}
