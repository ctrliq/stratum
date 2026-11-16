import { Alert, BadgeGroup } from '@ctrliq/quantic-react';

import type { NodeRow } from '../../../../types/nodes';
import { toUnavailableNotice } from '../../../../utils/formatters';
import { DriftBadge } from '../DriftBadge';

type Props = {
  reason: string | undefined;
  driftedRows: NodeRow[];
};

export function VersionNotice({ reason, driftedRows }: Props) {
  return (
    <Alert type="warning">
      {toUnavailableNotice(reason)}
      {driftedRows.length > 0 ? (
        <BadgeGroup>
          {driftedRows.map((row) => (
            <DriftBadge key={row.name} row={row} />
          ))}
        </BadgeGroup>
      ) : null}
    </Alert>
  );
}
