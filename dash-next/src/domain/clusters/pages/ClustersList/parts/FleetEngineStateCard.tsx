import { Card, CardMeta, CardTitle } from '@ctrliq/quantic-react';
import { useContext } from 'react';

import { useGetFleetEngineState } from '~/api/fleet/fleetComponents';
import {
  EngineStateFact,
  EngineStateFacts,
  EngineStateNote,
} from '~/domain/clusters/components/EngineStateFacts/EngineStateFacts';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { polledWhen } from '~/shared/query/polling';
import { toLocalTime } from '~/shared/utils/formatters';

import {
  toClusterCountText,
  toEvaluatedText,
  toQuotedRefusal,
  toSeverityTone,
  toUnknownCoverageText,
} from '../../../utils/formatters';
import { severityCountsOf } from '../../../utils/selectors';

export const FleetEngineStateCard = () => {
  const isLive = useContext(LiveRefreshContext);
  const { data, failureCount, failureReason, dataUpdatedAt } = useGetFleetEngineState(
    {},
    polledWhen(isLive),
  );
  const isRefused = failureCount > 0;
  return (
    <Card>
      <CardTitle slot="header">
        Fleet Engine State
        <span slot="subtitle">the hub&apos;s per-cluster severity roll-up</span>
      </CardTitle>
      {data?.complete === false ? (
        <CardMeta slot="header">
          <ToneBadge tone="warning">partial snapshot</ToneBadge>
        </CardMeta>
      ) : null}
      {data ? (
        <>
          <EngineStateFacts>
            <EngineStateFact term="Clusters">
              {toClusterCountText(data)}
              <small>{toEvaluatedText(data.evaluatedAt, dataUpdatedAt)}</small>
            </EngineStateFact>
            <EngineStateFact term="Clusters by severity">
              {severityCountsOf(data.countsBySeverity).map(([severity, count]) => (
                <ToneBadge key={severity} tone={toSeverityTone(severity)}>
                  {severity} {count}
                </ToneBadge>
              ))}
            </EngineStateFact>
            <EngineStateFact term="Required coverage">
              {data.requiredUnknownClusters > 0 ? (
                <ToneBadge tone="warning">{toUnknownCoverageText(data)}</ToneBadge>
              ) : (
                toUnknownCoverageText(data)
              )}
            </EngineStateFact>
            <EngineStateFact term="Evaluated">{toLocalTime(data.evaluatedAt)}</EngineStateFact>
          </EngineStateFacts>
          {isRefused ? (
            <EngineStateNote>
              {toQuotedRefusal('the latest read was refused', failureReason)}
            </EngineStateNote>
          ) : null}
        </>
      ) : (
        <EngineStateNote>
          {isRefused
            ? toQuotedRefusal('fleet engine state unavailable', failureReason)
            : 'reading fleet engine state…'}
        </EngineStateNote>
      )}
    </Card>
  );
};
