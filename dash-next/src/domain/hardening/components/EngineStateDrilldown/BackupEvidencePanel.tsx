import { toSeverityTone } from '~/domain/clusters/utils/formatters';

import type { BackupEvidence } from '../../types/evidence';
import {
  toAttemptLine,
  toBackupCaption,
  toCopyAge,
  toListingLine,
  toPolicyLines,
  toVerdictWord,
  toWarningText,
} from '../../utils/formatters';
import { toBackupReading, toDistinctBackups } from '../../utils/transforms';
import styles from './BackupEvidencePanel.module.scss';
import { Note } from './Note';
import { ToneText } from './ToneText';

type Props = {
  fragment: BackupEvidence;
  outcome: string | null | undefined;
  severity: string | undefined;
  now: number;
};

export const BackupEvidencePanel = ({ fragment, outcome, severity, now }: Props) => {
  const word = toVerdictWord(outcome);
  const reading = toBackupReading(fragment, now);
  return (
    <section className={styles.panel}>
      <p className={styles.verdict}>
        <ToneText tone={toSeverityTone(severity ?? '')}>{word}</ToneText>
        {word === reading.word ? <span> — {reading.detail}</span> : null}
      </p>
      <ul className={styles.lines}>
        {toPolicyLines(fragment.policy).map((line) => (
          <li key={line}>{line}</li>
        ))}
      </ul>
      {fragment.listing ? <p>{toListingLine(fragment.listing)}</p> : null}
      {(fragment.warnings ?? []).map((warning, index) => (
        <Note key={index}>{toWarningText(warning)}</Note>
      ))}
      {toDistinctBackups(fragment.backups, now).map((backup) => (
        <article key={backup.backupID} className={styles.backup}>
          <header>
            <strong>{backup.backupID}</strong>
            <span>{toBackupCaption(backup)}</span>
          </header>
          <ul className={styles.lines}>
            {backup.copies.map((copy) => (
              <li key={copy.key}>
                <p className={styles.copy}>
                  <span>{copy.copyID}</span>
                  <span>{copy.locationID ?? 'location unknown'}</span>
                  <span>{copy.kind ?? 'kind unknown'}</span>
                  <span>{toCopyAge(copy, now)}</span>
                </p>
                {copy.save ? <p>{toAttemptLine('save', copy.save)}</p> : null}
                {copy.upload ? <p>{toAttemptLine('upload', copy.upload)}</p> : null}
              </li>
            ))}
          </ul>
        </article>
      ))}
      {fragment.prune ? <p>{toAttemptLine('prune', fragment.prune)}</p> : null}
    </section>
  );
};
