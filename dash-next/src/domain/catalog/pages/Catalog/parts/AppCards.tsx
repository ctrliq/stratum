import { CardGroup, StatusBadge } from '@ctrliq/quantic-react';

import { ReleaseStatus } from '~/domain/catalog/components/ReleaseStatus';
import { essentialsCaption, essentialsTitle } from '~/domain/catalog/constants';
import type { AppListing } from '~/domain/catalog/types/catalog';
import {
  toChartLine,
  toInstalledAge,
  toStateText,
  toStatusText,
  toTimeoutText,
} from '~/domain/catalog/utils/formatters';
import { releaseStateOf } from '~/domain/catalog/utils/selectors';

import { AppCard } from './AppCard';
import styles from './AppCards.module.scss';
import { QuickInstall } from './QuickInstall';

export const AppCards = ({
  apps,
  releases,
  now,
  selectedId,
  canInstall,
  isBusy,
  onOpened,
  onQuickInstall,
}: AppListing) => (
  <CardGroup>
    {apps.map((app) => {
      const chart = releases.get(app.id);
      const state = chart === undefined ? undefined : releaseStateOf(chart, now);
      return (
        <AppCard
          key={app.id}
          app={app}
          subtitle={toChartLine(app)}
          isSelected={app.id === selectedId}
          onOpened={onOpened}
          footer={
            <>
              <span>{app.category}</span>
              {chart === undefined || state === undefined ? null : state === 'deployed' ? (
                <>
                  <StatusBadge size="sm" color="success">
                    Deployed
                  </StatusBadge>
                  <span>{toInstalledAge(chart, now)}</span>
                </>
              ) : (
                <ReleaseStatus state={state} text={toStateText(state, chart, now)} />
              )}
              {canInstall && chart === undefined ? (
                <QuickInstall
                  isBusy={isBusy}
                  onPressed={() => {
                    onQuickInstall(app);
                  }}
                />
              ) : null}
            </>
          }
        />
      );
    })}
  </CardGroup>
);

export const EssentialsStrip = ({
  apps,
  releases,
  now,
  selectedId,
  canInstall,
  isBusy,
  onOpened,
  onQuickInstall,
}: AppListing) => (
  <section className={styles.essentials} aria-labelledby="catalog-essentials">
    <hgroup>
      <h2 id="catalog-essentials" className={styles.title}>
        {essentialsTitle}
      </h2>
      <p className={styles.caption}>{essentialsCaption}</p>
    </hgroup>
    <CardGroup>
      {apps.map((app) => {
        const chart = releases.get(app.id);
        return (
          <AppCard
            key={app.id}
            app={app}
            subtitle={app.chart.version}
            isSelected={app.id === selectedId}
            onOpened={onOpened}
            footer={
              chart !== undefined ? (
                <ReleaseStatus
                  state={releaseStateOf(chart, now)}
                  text={toStatusText(releaseStateOf(chart, now), chart, now)}
                />
              ) : canInstall ? (
                <>
                  <span>defaults · {toTimeoutText(app)}</span>
                  <QuickInstall
                    isBusy={isBusy}
                    onPressed={() => {
                      onQuickInstall(app);
                    }}
                  />
                </>
              ) : null
            }
          />
        );
      })}
    </CardGroup>
  </section>
);
