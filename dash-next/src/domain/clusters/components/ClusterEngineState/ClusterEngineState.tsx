import { Card, CardMeta, CardTitle } from '@ctrliq/quantic-react';
import { skipToken } from '@tanstack/react-query';
import { useContext } from 'react';

import { useGetClusterEngineState } from '~/api/fleet/fleetComponents';
import type { StateSummary } from '~/api/fleet/fleetSchemas';
import { FindingsSurface } from '~/domain/hardening/components/FindingsSurface/FindingsSurface';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { polledWhen } from '~/shared/query/polling';
import { toLocalTime } from '~/shared/utils/formatters';

import {
  toCoverageText,
  toEngineStateRefusal,
  toEvaluatedText,
  toFindingCountsText,
  toFreshnessTone,
  toObservedText,
  toSeverityTone,
} from '../../utils/formatters';
import { coverageLabelsOf, occurringSeveritiesOf } from '../../utils/selectors';
import {
  EngineStateFact,
  EngineStateFacts,
  EngineStateNote,
} from '../EngineStateFacts/EngineStateFacts';

type Props = {
  clusterId: string | undefined;
};

type SummaryProps = {
  summary: StateSummary;
  readAt: number;
};

const SummaryFacts = ({ summary, readAt }: SummaryProps) => (
  <EngineStateFacts>
    <EngineStateFact term="Freshness">
      <ToneBadge tone={toFreshnessTone(summary.freshness)}>{summary.freshness}</ToneBadge>
      <small>{toEvaluatedText(summary.evaluatedAt, readAt)}</small>
    </EngineStateFact>
    <EngineStateFact term="Coverage">
      {toCoverageText(summary.coverage)}
      {coverageLabelsOf(summary.coverage, summary.observedAt).map(({ text, isWarning }) => (
        <ToneBadge key={text} tone={isWarning ? 'warning' : 'neutral'}>
          {text}
        </ToneBadge>
      ))}
    </EngineStateFact>
    <EngineStateFact term="Findings">
      {toFindingCountsText(summary.findingCounts)}
      {occurringSeveritiesOf(summary.findingCounts.bySeverity).map(([severity, count]) => (
        <ToneBadge key={severity} tone={toSeverityTone(severity)}>
          {severity} {count}
        </ToneBadge>
      ))}
    </EngineStateFact>
    <EngineStateFact term="Evaluated">{toLocalTime(summary.evaluatedAt)}</EngineStateFact>
    <EngineStateFact term="Observed">{toObservedText(summary.observedAt, readAt)}</EngineStateFact>
  </EngineStateFacts>
);

export const ClusterEngineState = ({ clusterId }: Props) => {
  const isLive = useContext(LiveRefreshContext);
  const { data, failureCount, failureReason, dataUpdatedAt } = useGetClusterEngineState(
    clusterId === undefined ? skipToken : { pathParams: { id: clusterId } },
    polledWhen(isLive),
  );
  const isRefused = failureCount > 0;
  return clusterId === undefined ? null : (
    <Card>
      <CardTitle slot="header">
        Engine State
        <span slot="subtitle">the hub&apos;s supervisor verdict</span>
      </CardTitle>
      {data ? (
        <CardMeta slot="header">
          <ToneBadge tone={toSeverityTone(data.severity)}>{data.severity}</ToneBadge>
        </CardMeta>
      ) : null}
      {data ? (
        <>
          <SummaryFacts summary={data} readAt={dataUpdatedAt} />
          {isRefused ? (
            <EngineStateNote>
              {toEngineStateRefusal('the latest read was refused', failureReason)}
            </EngineStateNote>
          ) : null}
          <FindingsSurface clusterId={clusterId} />
        </>
      ) : (
        <EngineStateNote>
          {isRefused
            ? toEngineStateRefusal('engine state unavailable', failureReason)
            : 'reading engine state…'}
        </EngineStateNote>
      )}
    </Card>
  );
};
