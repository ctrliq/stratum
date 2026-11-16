import { Panel, PanelHeader, Sparkline } from '@ctrliq/quantic-react';

import { absentText, toPercentText } from '~/shared/utils/formatters';

import { noUsageData, trendSamplesNeeded } from '../../../constants';
import { toCollectingText } from '../../../utils/formatters';
import styles from './UsageCard.module.scss';

type Props = {
  title: string;
  percent: number | undefined;
  caption: string;
  samples: number[];
};

export function UsageCard({ title, percent, caption, samples }: Props) {
  return (
    <Panel>
      <PanelHeader slot="header">
        {title}
        <span slot="subtitle">{percent === undefined ? absentText : caption}</span>
      </PanelHeader>
      {percent === undefined ? (
        <p className={styles.note}>{noUsageData}</p>
      ) : (
        <>
          <p className={styles.figure}>{toPercentText(percent)}</p>
          {samples.length < trendSamplesNeeded ? (
            <p className={styles.note}>{toCollectingText(samples.length)}</p>
          ) : (
            <figure className={styles.trend} aria-label={`${title} trend`}>
              <Sparkline values={samples} variant="gradient" yMin={0} yMax={100} />
            </figure>
          )}
        </>
      )}
    </Panel>
  );
}
