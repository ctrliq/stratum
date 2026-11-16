import {
  BadgeColor,
  BadgeSize,
  ButtonSize,
  ButtonVariant,
  IconSize,
} from '@ctrliq/quantic-components';
import {
  Badge,
  Button,
  Card,
  CardMeta,
  CardTitle,
  LucideIconX,
  Tooltip,
} from '@ctrliq/quantic-react';
import { useContext } from 'react';

import type { Operation } from '~/api/fleet/fleetSchemas';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';
import { WriteAccessContext } from '~/shared/context/WriteAccessContext';

import { cancelRequested } from '../../../constants';
import { useOperationCancel } from '../../../hooks/useOperationCancel';
import {
  toIdentityLine,
  toOutcomeSummary,
  toOutcomeTone,
  toSettledText,
} from '../../../utils/formatters';
import { isCancellable } from '../../../utils/predicates';
import styles from './OperationCard.module.scss';
import { OperationTargets } from './OperationTargets';

type Props = {
  operationId: string;
  operation: Operation;
  staleNote: string | undefined;
};

export const OperationCard = ({ operationId, operation, staleNote }: Props) => {
  const canManage = useContext(WriteAccessContext);
  const { isBusy, onCancelRequested } = useOperationCancel(operationId);
  return (
    <Card>
      <CardTitle slot="header">
        <code>{operationId}</code>
        {operation.cancelRequestedAt ? (
          <>
            {' '}
            <Badge color={BadgeColor.Purple} size={BadgeSize.Small}>
              {cancelRequested}
            </Badge>
          </>
        ) : null}
        <span slot="subtitle">{toIdentityLine(operation)}</span>
      </CardTitle>
      <CardMeta slot="header">
        <ToneBadge tone={toOutcomeTone(operation.outcome)}>{operation.outcome}</ToneBadge>
        {isCancellable(operation, canManage) ? (
          <Tooltip>
            <Button
              icon
              destructive
              variant={ButtonVariant.Ghost}
              size={ButtonSize.Small}
              aria-label="Cancel operation"
              disabled={isBusy}
              onClick={onCancelRequested}
            >
              <LucideIconX size={IconSize.Small} />
            </Button>
            <span slot="content">Request cancellation</span>
          </Tooltip>
        ) : null}
      </CardMeta>
      {staleNote ? (
        <p role="status" className={styles.note}>
          {staleNote}
        </p>
      ) : null}
      <dl className={styles.progress}>
        <dt>Settled</dt>
        <dd>{toSettledText(operation)}</dd>
        <dt>Committed</dt>
        <dd>{operation.committedCount ?? 0}</dd>
        <dt>Outcomes</dt>
        <dd>{toOutcomeSummary(operation.countsByOutcome)}</dd>
      </dl>
      <OperationTargets operation={operation} />
    </Card>
  );
};
