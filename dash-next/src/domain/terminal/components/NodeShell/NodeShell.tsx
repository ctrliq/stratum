import { Alert } from '@ctrliq/quantic-react';
import { skipToken } from '@tanstack/react-query';
import { useContext, useEffect, useState } from 'react';

import { useCreateBatchV1NamespacedJob, useListCoreV1NamespacedPod } from '~/api/k8s/k8sComponents';
import type { PodList } from '~/api/k8s/k8sSchemas';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { useServerConfig } from '~/shared/context/ConfigContext';

import {
  helperContainer,
  helperNamespace,
  helperPollLimit,
  helperPollMs,
  hostCommand,
  hostShellNeeds,
} from '../../constants';
import type { Helper } from '../../types/terminal';
import {
  toFailedShellLabel,
  toHelperLabel,
  toHostPlaceholder,
  toHostShellFailure,
} from '../../utils/formatters';
import { isAwaited } from '../../utils/predicates';
import { helperPodsOf, phaseOf } from '../../utils/selectors';
import { toHelperJob } from '../../utils/transforms';
import { ExecTerminal } from '../ExecTerminal/ExecTerminal';
import { SessionHeader } from '../SessionHeader/SessionHeader';
import styles from './NodeShell.module.scss';
import { deletedQuietlyOn, ownedOnce } from './ownedHelpers';

type Props = {
  node: string;
};

const podsIn = (list: PodList | undefined) => helperPodsOf(list?.items ?? []);

export function NodeShell({ node }: Props) {
  const clusterId = useContext(ClusterContext);
  const { features } = useServerConfig();
  const image = 'nodeShellImage' in features ? features.nodeShellImage : undefined;
  const { mutateAsync: created } = useCreateBatchV1NamespacedJob();
  const [helper, setHelper] = useState<Helper>();
  const [createError, setCreateError] = useState<unknown>();
  const [hasTimedOut, setHasTimedOut] = useState(false);

  useEffect(() => {
    const closing = new AbortController();
    const owning = image
      ? ownedOnce(
          clusterId,
          node,
          created({ pathParams: { namespace: helperNamespace }, body: toHelperJob(node, image) }),
        )
      : undefined;
    owning?.then(
      (owned) => {
        if (!closing.signal.aborted) {
          setHelper(owned);
        }
      },
      (error: unknown) => {
        if (!closing.signal.aborted) {
          setCreateError(error);
        }
      },
    );
    return () => {
      closing.abort();
      void owning?.then(deletedQuietlyOn, () => undefined);
    };
  }, [clusterId, node, image, created]);

  useEffect(() => {
    const waiting = helper
      ? setTimeout(() => {
          setHasTimedOut(true);
        }, helperPollMs * helperPollLimit)
      : undefined;
    return () => {
      clearTimeout(waiting);
    };
  }, [helper]);

  const pods = useListCoreV1NamespacedPod(
    helper
      ? {
          pathParams: { namespace: helperNamespace },
          queryParams: { labelSelector: helper.selector },
        }
      : skipToken,
    {
      retry: false,
      staleTime: 0,
      gcTime: 0,
      refetchOnWindowFocus: false,
      refetchOnReconnect: false,
      refetchInterval: ({ state }) =>
        isAwaited(phaseOf({ image, createError, pods: podsIn(state.data), hasTimedOut }))
          ? helperPollMs
          : false,
    },
  );
  const phase = phaseOf({ image, createError, pods: podsIn(pods.data), hasTimedOut });
  const hasFailed = phase.type === 'failed';

  useEffect(() => {
    if (hasFailed && helper) {
      void deletedQuietlyOn(helper);
    }
  }, [hasFailed, helper]);

  return phase.type === 'running' ? (
    <ExecTerminal
      target={{
        namespace: helperNamespace,
        pod: phase.pod,
        container: helperContainer,
        command: hostCommand,
      }}
    />
  ) : phase.type === 'failed' ? (
    <section className={styles.main} aria-label={`Host shell on ${node}`}>
      <SessionHeader status="error" label={toFailedShellLabel(node)} />
      <Alert type="error">{toHostShellFailure(phase.message)}</Alert>
      <p className={styles.needs}>{hostShellNeeds}</p>
    </section>
  ) : (
    <section className={styles.main} aria-label={`Host shell on ${node}`}>
      <SessionHeader
        status={phase.type}
        label={toHelperLabel(node, phase.type === 'waiting' ? phase.pod : undefined)}
      />
      <p className={styles.placeholder}>
        <code>{toHostPlaceholder(node)}</code>
      </p>
    </section>
  );
}
