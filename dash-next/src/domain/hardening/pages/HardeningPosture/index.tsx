import { IconSize } from '@ctrliq/quantic-components';
import { Alert, Button, LucideIconRefreshCw } from '@ctrliq/quantic-react';

import { useManualRefresh } from '~/domain/shell/hooks/useManualRefresh';
import { PageTitle } from '~/shared/components/PageTitle/PageTitle';
import { useServerConfig } from '~/shared/context/ConfigContext';

import {
  benchmarkCaption,
  benchmarkCommand,
  postureLoading,
  postureTitle,
  postureUnavailable,
} from '../../constants';
import { usePosture } from '../../hooks/usePosture';
import { toObservedCaption } from '../../utils/formatters';
import { cisProfileOf } from '../../utils/selectors';
import { toPostureCards, toPostureSignals } from '../../utils/transforms';
import styles from './HardeningPosture.module.scss';
import { ControlProfile } from './parts/ControlProfile';
import { ObservedSignals } from './parts/ObservedSignals';
import { PostureCards } from './parts/PostureCards';

export function HardeningPosture() {
  const { load, posture } = usePosture();
  const profile = cisProfileOf(useServerConfig());
  const onRefreshed = useManualRefresh();

  return (
    <article className={styles.page}>
      <header className={styles.header}>
        <hgroup>
          <PageTitle className={styles.title}>{postureTitle}</PageTitle>
          {load === 'ready' ? (
            <p>
              {benchmarkCaption}
              <code>{benchmarkCommand}</code>
            </p>
          ) : null}
        </hgroup>
        {load === 'loading' ? null : (
          <Button variant="outline" size="sm" onClick={onRefreshed}>
            <LucideIconRefreshCw size={IconSize.Small} />
            Refresh
          </Button>
        )}
      </header>
      {load === 'loading' ? <p role="status">{postureLoading}</p> : null}
      {load === 'unreachable' ? (
        <Alert type="warning">
          <span slot="title">{postureUnavailable}</span>
        </Alert>
      ) : null}
      {load === 'ready' ? (
        <>
          <PostureCards cards={toPostureCards(posture)} />
          <div className={styles.details}>
            <ObservedSignals
              caption={toObservedCaption(posture.observedAt)}
              signals={toPostureSignals(posture)}
            />
            <ControlProfile profile={profile} />
          </div>
        </>
      ) : null}
    </article>
  );
}
