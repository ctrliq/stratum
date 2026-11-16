import { Alert, Button, Spinner } from '@ctrliq/quantic-react';
import { useContext, useState } from 'react';

import type { HelmChart } from '~/api/k8s/k8sSchemas';
import { AppDetailFrame } from '~/domain/catalog/components/AppDetailFrame';
import { AppSummary } from '~/domain/catalog/components/AppSummary';
import { ReleaseStatus } from '~/domain/catalog/components/ReleaseStatus';
import { ValueList } from '~/domain/catalog/components/ValueList';
import { helmChartsScreen } from '~/domain/catalog/constants';
import { InstallForm } from '~/domain/catalog/forms/InstallForm';
import type { CatalogApp, InstallValues } from '~/domain/catalog/types/catalog';
import {
  toInstalledAge,
  toPhaseText,
  toStateText,
  toStateTone,
  toStuckWarning,
  toUninstallingCaption,
} from '~/domain/catalog/utils/formatters';
import { hasCredentials } from '~/domain/catalog/utils/predicates';
import {
  configuredValuesOf,
  extraStatusOf,
  releaseStateOf,
  statusTextOf,
} from '~/domain/catalog/utils/selectors';
import { toSeedValues } from '~/domain/catalog/utils/transforms';
import { useScreenNavigation } from '~/domain/shell/hooks/useScreenNavigation';
import { toClusterPath, withNamespace } from '~/domain/shell/utils/transforms';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { NamespaceContext } from '~/shared/context/NamespaceContext';

import styles from './AppDetail.module.scss';

type Props = {
  app: CatalogApp;
  chart: HelmChart | undefined;
  now: number;
  isStateKnown: boolean;
  canWrite: boolean;
  isBusy: boolean;
  isInstalling: boolean;
  onInstalled: (app: CatalogApp, values: InstallValues) => void;
  onUninstallRequested: (app: CatalogApp, chart: HelmChart) => void;
  onClosed: () => void;
};

type ReleaseProps = Pick<Props, 'app' | 'now' | 'canWrite' | 'isBusy' | 'onClosed'> & {
  chart: HelmChart;
  onUninstallRequested: () => void;
};

const InstalledApp = ({
  app,
  chart,
  now,
  canWrite,
  isBusy,
  onUninstallRequested,
  onClosed,
}: ReleaseProps) => {
  const clusterId = useContext(ClusterContext) ?? '';
  const namespace = useContext(NamespaceContext) ?? '';
  const { onOpened } = useScreenNavigation();
  const state = releaseStateOf(chart, now);
  const message = statusTextOf(chart, 'message');
  const configured = configuredValuesOf(app, chart);
  return (
    <AppDetailFrame
      app={app}
      onClosed={onClosed}
      status={<ReleaseStatus state={state} text={toStateText(state, chart, now)} />}
      footer={
        state === 'removing' ? (
          <Button size="xs" variant="outline" disabled>
            <Spinner size="xs" label="Uninstalling" />
            {toUninstallingCaption(chart, now)}
          </Button>
        ) : canWrite ? (
          <Button
            size="xs"
            variant="outline"
            destructive
            disabled={isBusy}
            onClick={onUninstallRequested}
          >
            {state === 'stuck' ? 'Retry uninstall' : 'Uninstall'}
          </Button>
        ) : null
      }
    >
      <AppSummary app={app} targetNamespace={chart.spec?.targetNamespace ?? ''} />
      {state === 'failed' && message ? <Alert type="error">{message}</Alert> : null}
      {state === 'stuck' ? <Alert type="error">{toStuckWarning(chart, now)}</Alert> : null}
      <ValueList
        title="Release status"
        rows={[
          { key: 'Phase', value: toPhaseText(state, chart, now), tone: toStateTone(state) },
          { key: 'Revision', value: statusTextOf(chart, 'revision') },
          { key: 'Chart version', value: statusTextOf(chart, 'chartVersion') },
          { key: 'Release', value: statusTextOf(chart, 'releaseName') },
          { key: 'Release namespace', value: statusTextOf(chart, 'releaseNamespace') },
          { key: 'Installed', value: toInstalledAge(chart, now) },
          ...(state !== 'failed' && message ? [{ key: 'Message', value: message }] : []),
        ]}
      />
      {extraStatusOf(chart).length > 0 ? (
        <ul className={styles.extraStatus}>
          {extraStatusOf(chart).map(({ key, value }) => (
            <li key={key}>
              {key}: {value}
            </li>
          ))}
        </ul>
      ) : null}
      {configured.length > 0 ? <ValueList title="Configured values" rows={configured} /> : null}
      {hasCredentials(app) ? null : (
        <Button
          className={styles.resourceLink}
          variant="ghost"
          size="sm"
          onClick={() => {
            onOpened(
              withNamespace(`${toClusterPath(clusterId)}/${helmChartsScreen}`, '', namespace),
            );
          }}
        >
          View HelmChart resource
        </Button>
      )}
    </AppDetailFrame>
  );
};

export const AppDetail = ({
  app,
  chart,
  now,
  isStateKnown,
  canWrite,
  isBusy,
  isInstalling,
  onInstalled,
  onUninstallRequested,
  onClosed,
}: Props) => {
  const [submitted, setSubmitted] = useState<InstallValues>();
  return chart === undefined ? (
    <InstallForm
      app={app}
      initialValues={submitted ?? toSeedValues(app)}
      isStateKnown={isStateKnown}
      canWrite={canWrite}
      isBusy={isBusy}
      isInstalling={isInstalling}
      onClosed={onClosed}
      onReset={() => {
        setSubmitted(undefined);
      }}
      onInstalled={(values) => {
        setSubmitted(values);
        onInstalled(app, values);
      }}
    />
  ) : (
    <InstalledApp
      app={app}
      chart={chart}
      now={now}
      canWrite={canWrite}
      isBusy={isBusy}
      onClosed={onClosed}
      onUninstallRequested={() => {
        onUninstallRequested(app, chart);
      }}
    />
  );
};
