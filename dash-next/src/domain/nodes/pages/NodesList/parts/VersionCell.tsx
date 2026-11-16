import { LucideIconCheck } from '@ctrliq/quantic-react';

import type { NodeRow } from '../../../types/nodes';
import { isRowDrifted, isStamped } from '../../../utils/predicates';
import { DriftBadge } from './DriftBadge';
import styles from './VersionCell.module.scss';

type Props = {
  row: NodeRow;
};

export function VersionCell({ row }: Props) {
  return (
    <span className={styles.main}>
      {row.kubeletVersion}
      {isRowDrifted(row) ? (
        <DriftBadge row={row} />
      ) : isStamped(row) ? (
        <LucideIconCheck className={styles.aligned} size="sm" aria-label="Aligned" />
      ) : null}
    </span>
  );
}
