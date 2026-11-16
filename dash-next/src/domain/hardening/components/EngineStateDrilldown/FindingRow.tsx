import { Button } from '@ctrliq/quantic-react';
import clsx from 'clsx';

import { toSeverityTone } from '~/domain/clusters/utils/formatters';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';
import { toDisplayText } from '~/shared/utils/formatters';

import type { FindingView, ObservationView } from '../../types/engineState';
import {
  toCollectionTimes,
  toFindingDetail,
  toProcessLine,
  toReasonText,
  toSeverityText,
} from '../../utils/formatters';
import { hasEvidence } from '../../utils/predicates';
import { FactList } from './FactList';
import styles from './FindingRow.module.scss';

type Props = {
  finding: FindingView;
  observation: ObservationView | undefined;
  isOpen: boolean;
  onEvidenceRequested: () => void;
};

const CollectionObservation = ({ observation }: { observation: ObservationView }) => (
  <section className={styles.collection}>
    <p>Collection: {observation.availability}</p>
    {observation.reason ? <p>{toReasonText(observation.reason)}</p> : null}
    {observation.source ? <p>{toProcessLine(observation.source)}</p> : null}
    <FactList facts={toCollectionTimes(observation)} />
  </section>
);

export const FindingRow = ({ finding, observation, isOpen, onEvidenceRequested }: Props) => (
  <li
    className={clsx(styles.finding, { [styles.stateOpen]: isOpen })}
    aria-current={isOpen ? 'true' : undefined}
  >
    <ToneBadge tone={toSeverityTone(finding.severity ?? '')}>{toSeverityText(finding)}</ToneBadge>
    <code>{toDisplayText(finding.code)}</code>
    {finding.message ? <span className={styles.message}>{finding.message}</span> : null}
    <span className={styles.detail}>{toFindingDetail(finding)}</span>
    {hasEvidence(finding) ? (
      <Button variant="ghost" size="sm" onClick={onEvidenceRequested}>
        Evidence
      </Button>
    ) : null}
    {observation ? <CollectionObservation observation={observation} /> : null}
  </li>
);
