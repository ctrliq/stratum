import { toJson } from '~/shared/utils/formatters';

import { backupSignal, hubDerivedEvidence, readingEvidence, supervisorType } from '../../constants';
import type { EvidencePageView, OpenedEvidence, Refusal } from '../../types/engineState';
import {
  toEvidenceRefusal,
  toEvidenceTitle,
  toPartialEvidenceNote,
  toRawItemTitle,
  toSourceFacts,
} from '../../utils/formatters';
import { isShownRaw, isTypedSignal } from '../../utils/predicates';
import { itemsOf, itemsOfType } from '../../utils/selectors';
import { toCertificateRows } from '../../utils/transforms';
import { BackupEvidencePanel } from './BackupEvidencePanel';
import { CertificateEvidencePanel } from './CertificateEvidencePanel';
import styles from './EvidenceDrawer.module.scss';
import { FactList } from './FactList';
import { Note } from './Note';
import { SupervisorEvidence } from './SupervisorEvidence';

type Props = {
  opened: OpenedEvidence;
  page: EvidencePageView | undefined;
  refusal: Refusal | undefined;
  isReading: boolean;
  now: number;
};

type BodyProps = Pick<Props, 'opened' | 'now'> & {
  page: EvidencePageView;
};

const EvidenceBody = ({ opened, page, now }: BodyProps) => {
  const items = itemsOf(page);
  const certificateRows = toCertificateRows(opened, items);
  const backupFragments = opened.signal === backupSignal ? itemsOfType(items, backupSignal) : [];
  return (
    <>
      {page.source ? (
        <section>
          <h4>Source</h4>
          <FactList facts={toSourceFacts(page.source, now)} />
        </section>
      ) : (
        <p>{hubDerivedEvidence}</p>
      )}
      {certificateRows.length > 0 ? (
        <CertificateEvidencePanel rows={certificateRows} now={now} />
      ) : null}
      {backupFragments.map((fragment, index) => (
        <BackupEvidencePanel
          key={index}
          fragment={fragment}
          outcome={opened.observation?.nativeOutcome}
          severity={opened.severity}
          now={now}
        />
      ))}
      {items.map((item, index) =>
        item.type === supervisorType ? (
          <SupervisorEvidence key={index} item={item} now={now} />
        ) : isShownRaw(item, opened.signal) ? (
          <figure key={index} className={styles.rawItem}>
            <figcaption>{toRawItemTitle(item.type)}</figcaption>
            <pre>{toJson(item)}</pre>
          </figure>
        ) : null,
      )}
      {isTypedSignal(opened.signal) ? (
        <details>
          <summary>Full evidence</summary>
          <pre>{toJson(page)}</pre>
        </details>
      ) : null}
    </>
  );
};

export const EvidenceDrawer = ({ opened, page, refusal, isReading, now }: Props) => (
  <section className={styles.drawer}>
    <h3>{toEvidenceTitle(opened.evidenceRef)}</h3>
    {isReading ? <p role="status">{readingEvidence}</p> : null}
    {refusal === undefined ? null : <Note>{toEvidenceRefusal(refusal)}</Note>}
    {page?.complete === false ? <Note>{toPartialEvidenceNote(page)}</Note> : null}
    {page === undefined ? null : <EvidenceBody opened={opened} page={page} now={now} />}
  </section>
);
