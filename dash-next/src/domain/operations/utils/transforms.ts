import { absentText, toLocalTime } from '~/shared/utils/formatters';

import type { TargetAnswer, TargetRow } from '../types';

export const toTargetRow = (
  { childOperationID, target, state, outcome, committed, completedAt }: TargetAnswer,
  index: number,
): TargetRow => ({
  id: childOperationID || String(index),
  cluster: target?.cluster?.name || absentText,
  node: target?.nodeUID || absentText,
  state: state || absentText,
  outcome: outcome ?? '',
  committed: committed ? 'yes' : 'no',
  completed: toLocalTime(completedAt),
});
