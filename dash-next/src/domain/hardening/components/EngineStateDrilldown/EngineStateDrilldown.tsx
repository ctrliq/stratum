import { Button, Panel, PanelHeader } from '@ctrliq/quantic-react';
import { type ReactNode, useState } from 'react';

import { toSeverityTone } from '~/domain/clusters/utils/formatters';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';
import { toDisplayText } from '~/shared/utils/formatters';

import { allFilters, drilldownSubtitle, readingEngineState } from '../../constants';
import { useEngineState } from '../../hooks/useEngineState';
import { useEvidence } from '../../hooks/useEvidence';
import { useNow } from '../../hooks/useNow';
import type {
  FindingFilters as Filters,
  FindingView,
  OpenedEvidence,
} from '../../types/engineState';
import {
  toDrilldownTitle,
  toPreviousEvidenceNote,
  toRetainedFiltersNote,
  toSummaryRefusal,
} from '../../utils/formatters';
import { isEvidenceOpen, isSameEvidence, isSameFilters } from '../../utils/predicates';
import { withOpenedFinding } from '../../utils/reducers';
import {
  certificateObservationsOf,
  currentGenerationOf,
  observationFor,
} from '../../utils/selectors';
import { toWalkedRoleCoverage } from '../../utils/transforms';
import styles from './EngineStateDrilldown.module.scss';
import { EvidenceDrawer } from './EvidenceDrawer';
import { FindingFilters } from './FindingFilters';
import { FindingRow } from './FindingRow';
import { FindingsStatus } from './FindingsStatus';
import { Note } from './Note';
import { StateSummary } from './StateSummary';

type Props = {
  clusterId: string;
  clusterName?: string;
  onClosed: () => void;
  children?: ReactNode;
};

const ClusterDrilldown = ({ clusterId, clusterName, onClosed, children }: Props) => {
  const [filters, setFilters] = useState<Filters>(allFilters);
  const [opened, setOpened] = useState<OpenedEvidence>();
  const now = useNow();
  const { summary, summaryRefusal, walk, findingsRefusal, onSummaryReread, onReread } =
    useEngineState(clusterId, filters);
  const evidence = useEvidence(clusterId, opened);
  const observations = walk?.observations ?? [];

  const onEvidenceRequested = (finding: FindingView): void => {
    const next = walk === undefined ? opened : withOpenedFinding(opened, finding, walk);
    setOpened(next);
    if (isSameEvidence(opened, next)) {
      evidence.onReread();
    }
  };

  return (
    <Panel>
      <PanelHeader slot="header">
        {toDrilldownTitle(clusterName, clusterId)}
        <span slot="subtitle">{drilldownSubtitle}</span>
        <span slot="meta" className={styles.headerActions}>
          {summary === undefined ? null : (
            <ToneBadge tone={toSeverityTone(summary.severity ?? '')}>
              {toDisplayText(summary.severity)}
            </ToneBadge>
          )}
          <Button variant="ghost" size="sm" onClick={onClosed}>
            Close
          </Button>
        </span>
      </PanelHeader>
      <div className={styles.body}>
        {children}
        {summary === undefined ? (
          summaryRefusal === undefined ? (
            <p role="status">{readingEngineState}</p>
          ) : (
            <Note>{toSummaryRefusal(summaryRefusal, false)}</Note>
          )
        ) : (
          <>
            <StateSummary
              summary={summary}
              certificateRoles={toWalkedRoleCoverage(certificateObservationsOf(observations))}
              now={now}
            />
            {summaryRefusal === undefined ? null : (
              <Note>{toSummaryRefusal(summaryRefusal, true)}</Note>
            )}
            <FindingFilters
              filters={filters}
              findings={walk?.findings ?? []}
              onFiltersChanged={(selected) => {
                setFilters(selected);
                onSummaryReread();
              }}
            />
            {walk === undefined || isSameFilters(walk.filters, filters) ? null : (
              <Note>{toRetainedFiltersNote(walk.filters, findingsRefusal === undefined)}</Note>
            )}
            <FindingsStatus
              summary={summary}
              walk={walk}
              refusal={findingsRefusal}
              onReread={onReread}
            />
            {walk === undefined || walk.findings.length === 0 ? null : (
              <ul className={styles.findings}>
                {walk.findings.map((finding, index) => (
                  <FindingRow
                    key={index}
                    finding={finding}
                    observation={observationFor(observations, finding)}
                    isOpen={isEvidenceOpen(finding, opened)}
                    onEvidenceRequested={() => {
                      onEvidenceRequested(finding);
                    }}
                  />
                ))}
              </ul>
            )}
            {opened === undefined ? null : (
              <>
                {opened.reportGeneration ===
                currentGenerationOf(summary, findingsRefusal) ? null : (
                  <Note>{toPreviousEvidenceNote(opened.reportGeneration)}</Note>
                )}
                <EvidenceDrawer
                  opened={opened}
                  page={evidence.page}
                  refusal={evidence.refusal}
                  isReading={evidence.isReading}
                  now={now}
                />
              </>
            )}
          </>
        )}
      </div>
    </Panel>
  );
};

export const EngineStateDrilldown = (props: Props) => (
  <ClusterDrilldown key={props.clusterId} {...props} />
);
