import { Button } from '@ctrliq/quantic-react';

import type { FindingsWalk, Refusal, SummaryView } from '../../types/engineState';
import {
  toFindingsHeader,
  toFindingsRefusal,
  toPartialFindingsNote,
  toPreviousReportNote,
} from '../../utils/formatters';
import styles from './FindingsStatus.module.scss';
import { Note } from './Note';

type Props = {
  summary: SummaryView;
  walk: FindingsWalk | undefined;
  refusal: Refusal | undefined;
  onReread: () => void;
};

const reportChanged = 409;

export const FindingsStatus = ({ summary, walk, refusal, onReread }: Props) => (
  <>
    {walk === undefined ? null : <p className={styles.header}>{toFindingsHeader(walk)}</p>}
    {walk === undefined || walk.isComplete ? null : <Note>{toPartialFindingsNote(walk)}</Note>}
    {walk === undefined || walk.reportGeneration === summary.reportGeneration ? null : (
      <Note>{toPreviousReportNote(walk.reportGeneration, summary.reportGeneration)}</Note>
    )}
    {refusal === undefined ? null : (
      <Note
        action={
          refusal.status === reportChanged ? (
            <Button slot="action" variant="outline" size="sm" onClick={onReread}>
              Reload findings
            </Button>
          ) : null
        }
      >
        {toFindingsRefusal(refusal)}
      </Note>
    )}
  </>
);
