import { Card, CardTitle } from '@ctrliq/quantic-react';

import { useOperation } from '../../../hooks/useOperation';
import { toReadFailure, toStaleNote } from '../../../utils/formatters';
import { isGone } from '../../../utils/predicates';
import { OperationCard } from './OperationCard';

type Props = {
  operationId: string;
};

export const TrackedOperation = ({ operationId }: Props) => {
  const { data, failureCount, failureReason } = useOperation(operationId);
  const isFailing = failureCount > 0;
  return isFailing && (isGone(failureReason) || data === undefined) ? (
    <Card>
      <CardTitle slot="header">
        <code>{operationId}</code>
      </CardTitle>
      <p role="status">{toReadFailure(failureReason)}</p>
    </Card>
  ) : data === undefined ? (
    <Card>
      <p role="status">Loading {operationId}…</p>
    </Card>
  ) : (
    <OperationCard
      operationId={operationId}
      operation={data}
      staleNote={isFailing ? toStaleNote(failureReason) : undefined}
    />
  );
};
