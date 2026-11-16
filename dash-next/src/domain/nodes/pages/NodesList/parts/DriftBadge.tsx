import { toDriftMarker } from '~/domain/nodes/utils/formatters';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';

import type { NodeRow } from '../../../types/nodes';
import { toDriftTitle } from '../../../utils/formatters';

type Props = {
  row: Pick<NodeRow, 'stratumVersion' | 'kubeletVersion'>;
};

export function DriftBadge({ row }: Props) {
  return (
    <ToneBadge tone="warning" hint={toDriftTitle(row)}>
      {toDriftMarker(row.stratumVersion)}
    </ToneBadge>
  );
}
