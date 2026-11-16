import { Tooltip } from '@ctrliq/quantic-react';
import type { CellContext } from '@tanstack/react-table';

import type { AgedWorkloadRow } from '../../../types/workloads';
import { toObservedText } from '../../../utils/formatters';

export function ReadyCell({ row }: CellContext<AgedWorkloadRow, unknown>) {
  const { readyCount, observedAt } = row.original;
  return observedAt ? (
    <Tooltip>
      <span>{readyCount}</span>
      <span slot="content">{toObservedText(observedAt)}</span>
    </Tooltip>
  ) : (
    readyCount
  );
}
