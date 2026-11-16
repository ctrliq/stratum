import { Tooltip } from '@ctrliq/quantic-react';

import {
  EngineStateFact,
  EngineStateFacts,
} from '~/domain/clusters/components/EngineStateFacts/EngineStateFacts';
import {
  toCoverageText,
  toEvaluatedText,
  toFindingCountsText,
  toFreshnessTone,
  toSeverityTone,
} from '~/domain/clusters/utils/formatters';
import { coverageLabelsOf, occurringSeveritiesOf } from '~/domain/clusters/utils/selectors';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';
import { toDisplayText } from '~/shared/utils/formatters';

import { coverageHint } from '../../constants';
import type { RoleCoverage, SummaryView } from '../../types/engineState';
import { RoleCoverageList } from './RoleCoverageList';
import { ToneText } from './ToneText';

type Props = {
  summary: SummaryView;
  certificateRoles: RoleCoverage[] | undefined;
  now: number;
};

export const StateSummary = ({ summary, certificateRoles, now }: Props) => (
  <EngineStateFacts>
    <EngineStateFact term="Freshness">
      <ToneBadge tone={toFreshnessTone(summary.freshness ?? '')}>
        {toDisplayText(summary.freshness)}
      </ToneBadge>
      <ToneText tone="neutral">{toEvaluatedText(summary.evaluatedAt, now)}</ToneText>
    </EngineStateFact>
    <EngineStateFact term="Coverage">
      {toCoverageText(summary.coverage)}
      {coverageLabelsOf(summary.coverage, summary.observedAt).map(({ text, isWarning }) => (
        <Tooltip key={text}>
          <ToneText tone={isWarning ? 'warning' : 'neutral'}>{text}</ToneText>
          <span slot="content">{coverageHint}</span>
        </Tooltip>
      ))}
    </EngineStateFact>
    <EngineStateFact term="Findings">
      {toFindingCountsText(summary.findingCounts)}
      {occurringSeveritiesOf(summary.findingCounts?.bySeverity).map(([severity, count]) => (
        <ToneBadge key={severity} tone={toSeverityTone(severity)}>
          {severity} {count}
        </ToneBadge>
      ))}
    </EngineStateFact>
    {certificateRoles === undefined ? null : (
      <EngineStateFact term="Certificate coverage by role">
        {certificateRoles.length > 0 ? <RoleCoverageList roles={certificateRoles} /> : null}
      </EngineStateFact>
    )}
  </EngineStateFacts>
);
