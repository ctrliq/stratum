import type { CellContext } from '@tanstack/react-table';

import type { AgedWorkloadRow } from '../../../types/workloads';
import { screenOfKind } from '../../../utils/selectors';
import styles from './NameCell.module.scss';

export function NameCell({ row }: CellContext<AgedWorkloadRow, unknown>) {
  const KindIcon = screenOfKind(row.original.kind)?.icon;
  return (
    <span className={styles.main}>
      {KindIcon ? <KindIcon size="sm" /> : null}
      {row.original.name}
    </span>
  );
}
