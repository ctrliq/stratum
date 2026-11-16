import { fetchDeleteBatchV1NamespacedJob } from '~/api/k8s/k8sComponents';
import type { Job } from '~/api/k8s/k8sSchemas';
import { sessionQueryKey } from '~/domain/auth/hooks/useSession';
import { csrfTokenOf } from '~/domain/auth/utils/selectors';
import { hasStatus, requestDeadlineMs } from '~/shared/fetchers/requests';
import { queryClient } from '~/shared/query/client';
import { nameOf } from '~/shared/utils/selectors';

import { helperDeleteGraceSeconds, helperNamespace } from '../../constants';
import type { Helper } from '../../types/terminal';
import { toHelperWarning } from '../../utils/formatters';
import { withHelper, withoutHelper } from '../../utils/reducers';
import { selectorOf } from '../../utils/selectors';

const notFound = 404;

const state: { owned: Helper[]; creating: Promise<unknown>[] } = { owned: [], creating: [] };

const ownerNow = (): string => csrfTokenOf(queryClient.getQueryData(sessionQueryKey));

const onForgotten = (helper: Helper): void => {
  state.owned = withoutHelper(state.owned, helper);
};

export const ownedOnce = (
  clusterId: string | undefined,
  node: string,
  creating: Promise<Job>,
): Promise<Helper> => {
  const owner = ownerNow();
  const owning = creating.then((job) => {
    const helper = { clusterId, node, job: nameOf(job), selector: selectorOf(job), owner };
    state.owned = withHelper(state.owned, helper);
    return helper;
  });
  const onSettled = (): void => {
    state.creating = state.creating.filter((pending) => pending !== owning);
  };
  state.creating = [...state.creating, owning];
  owning.then(onSettled, onSettled);
  return owning;
};

type Release = {
  keepalive?: boolean;
};

export const deletedOn = (
  helper: Helper,
  deadlineMs: number = requestDeadlineMs,
  { keepalive }: Release = {},
): Promise<void> =>
  state.owned.includes(helper) && helper.owner === ownerNow()
    ? fetchDeleteBatchV1NamespacedJob({
        pathParams: { namespace: helperNamespace, name: helper.job },
        queryParams: {
          gracePeriodSeconds: helperDeleteGraceSeconds,
          propagationPolicy: 'Background',
        },
        clusterId: helper.clusterId,
        deadlineMs,
        keepalive,
      }).then(
        () => {
          onForgotten(helper);
        },
        (error: unknown) =>
          hasStatus(error, notFound) ? onForgotten(helper) : Promise.reject(error),
      )
    : Promise.resolve();

export const deletedQuietlyOn = (helper: Helper): Promise<void> =>
  deletedOn(helper).catch(() => undefined);

const settledWithin = (pending: Promise<unknown>[], deadlineMs: number): Promise<unknown> =>
  Promise.race([
    Promise.allSettled(pending),
    new Promise((resolve) => {
      setTimeout(resolve, deadlineMs);
    }),
  ]);

export const releasedWithin = (deadlineMs: number, release: Release = {}): Promise<void> =>
  settledWithin(state.creating, deadlineMs)
    .then(() =>
      Promise.all(
        state.owned.map((helper) =>
          deletedOn(helper, deadlineMs, release).catch(() => {
            console.warn(toHelperWarning(helper.node, helper.job));
          }),
        ),
      ),
    )
    .then(() => undefined);
