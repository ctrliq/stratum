import { toDisplayText } from '~/shared/utils/formatters';

import { certificatePanelCaption, certificatePanelTitle, noRenewalRecord } from '../../constants';
import type { CertificateRow } from '../../types/evidence';
import {
  toCertificateFacts,
  toExpiryLine,
  toExpiryTone,
  toObservationFacts,
  toRenewalFacts,
  toSampleLine,
} from '../../utils/formatters';
import { isReadStanding } from '../../utils/predicates';
import { toCertificateCauses, toRoleCoverage } from '../../utils/transforms';
import styles from './CertificateEvidencePanel.module.scss';
import { FactList } from './FactList';
import { RoleCoverageList } from './RoleCoverageList';
import { ToneText } from './ToneText';

type Props = {
  rows: CertificateRow[];
  now: number;
};

type BlockProps = {
  row: CertificateRow;
  now: number;
};

const CertificateBlock = ({ row, now }: BlockProps) => {
  const { observation, evidence } = row;
  const isStanding = isReadStanding(observation);
  const notAfter = isStanding ? evidence?.notAfter : undefined;
  const expiry = toExpiryLine(notAfter, now);
  return (
    <article className={styles.certificate}>
      {observation === undefined ? null : (
        <header>
          <code>{toDisplayText(observation.id)}</code>
          {observation.subject?.nodeUID ? <span>node {observation.subject.nodeUID}</span> : null}
        </header>
      )}
      <FactList
        facts={[
          ...toCertificateFacts(evidence, isStanding),
          ...(observation === undefined ? [] : toObservationFacts(observation)),
        ]}
      />
      {expiry === undefined ? null : (
        <ToneText tone={toExpiryTone(notAfter, now)}>{expiry}</ToneText>
      )}
      {observation === undefined ? null : <p>{toSampleLine(observation.times?.sampledAt, now)}</p>}
      {evidence?.renewal ? (
        <FactList facts={toRenewalFacts(evidence.renewal)} />
      ) : (
        <p>{noRenewalRecord}</p>
      )}
      {toCertificateCauses(row).map((cause) => (
        <p key={cause} className={styles.cause}>
          {cause}
        </p>
      ))}
    </article>
  );
};

export const CertificateEvidencePanel = ({ rows, now }: Props) => (
  <section className={styles.panel}>
    <header>
      <h4>{certificatePanelTitle}</h4>
      <span>{certificatePanelCaption}</span>
    </header>
    <RoleCoverageList roles={toRoleCoverage(rows)} />
    {rows.map((row, index) => (
      <CertificateBlock key={index} row={row} now={now} />
    ))}
  </section>
);
