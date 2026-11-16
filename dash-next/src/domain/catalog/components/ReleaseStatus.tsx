import { Spinner, StatusBadge } from '@ctrliq/quantic-react';

import type { ReleaseState } from '../types/catalog';
import { isInFlight } from '../utils/predicates';
import styles from './ReleaseStatus.module.scss';

type Props = {
  state: ReleaseState;
  text: string;
};

export const ReleaseStatus = ({ state, text }: Props) =>
  isInFlight(state) ? (
    <span className={styles.progress}>
      <Spinner size="xs" label={text} />
      <span aria-hidden="true">{text}</span>
    </span>
  ) : (
    <StatusBadge size="sm" color={state === 'deployed' ? 'success' : 'error'}>
      {text}
    </StatusBadge>
  );
