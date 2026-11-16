import { Hinted } from '~/shared/components/Hinted/Hinted';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';

import { intentUnknownHint } from '../../../../constants';
import type { VersionRow } from '../../../../types/versions';
import {
  toCauseTitle,
  toReasonText,
  toRolloutText,
  toRolloutTitle,
  toVerdictText,
} from '../../../../utils/formatters';
import { isIntentUnknown, isOutcomeCause } from '../../../../utils/predicates';
import styles from './VerdictCell.module.scss';

type Props = {
  row: VersionRow;
};

const loudSeverities = ['warning', 'critical'];

export function VerdictCell({ row }: Props) {
  return (
    <span className={styles.main}>
      <Hinted hint={row.comparisonReason ? toReasonText(row.comparisonReason) : undefined}>
        <span>{toVerdictText(row)}</span>
      </Hinted>
      {row.intentLabel === undefined ? null : (
        <>
          <ToneBadge tone="progress" hint={toRolloutTitle(row)}>
            {toRolloutText(row)}
          </ToneBadge>
          {row.intent?.deadline ? <time>{row.intent.deadline}</time> : null}
        </>
      )}
      {isIntentUnknown(row) ? (
        <Hinted hint={intentUnknownHint}>
          <small>intent unknown</small>
        </Hinted>
      ) : null}
      {row.causes
        .filter((cause) => !isOutcomeCause(cause))
        .map((cause) => (
          <ToneBadge
            key={cause.code}
            tone={loudSeverities.includes(cause.severity ?? '') ? 'warning' : 'neutral'}
            hint={toCauseTitle(cause)}
          >
            {cause.code}
          </ToneBadge>
        ))}
    </span>
  );
}
