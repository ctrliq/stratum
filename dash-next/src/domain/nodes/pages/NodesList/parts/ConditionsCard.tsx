import { Panel, PanelHeader } from '@ctrliq/quantic-react';

import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';

import { noNodeData } from '../../../constants';
import type { PressureCondition } from '../../../types/nodes';
import { toPressureBadge } from '../../../utils/transforms';
import styles from './ConditionsCard.module.scss';

type Props = {
  conditions: PressureCondition[] | undefined;
};

export function ConditionsCard({ conditions }: Props) {
  return (
    <Panel>
      <PanelHeader slot="header">Conditions</PanelHeader>
      {conditions === undefined ? (
        <p className={styles.note}>{noNodeData}</p>
      ) : (
        <dl className={styles.conditions}>
          {conditions.map((condition) => {
            const badge = toPressureBadge(condition);
            return (
              <div key={condition.type} className={styles.condition}>
                <dt>{condition.type}</dt>
                <dd>
                  <ToneBadge tone={badge.tone} hint={badge.detail}>
                    {badge.text}
                  </ToneBadge>
                </dd>
              </div>
            );
          })}
        </dl>
      )}
    </Panel>
  );
}
