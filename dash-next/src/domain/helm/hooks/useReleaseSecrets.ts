import { skipToken } from '@tanstack/react-query';
import { useContext, useMemo, useState } from 'react';

import { useListSecretSummaries } from '~/api/dashboard/dashboardComponents';
import { useListCoreV1SecretForAllNamespaces } from '~/api/k8s/k8sComponents';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { type LoadState, toLoadState } from '~/shared/query/loadState';
import { polledWhen } from '~/shared/query/polling';
import type { KubeObject } from '~/shared/types/kubernetes';
import { hasFeature } from '~/shared/utils/predicates';

import { metadataTableAccept, releaseReadMeta, releaseSecretSelector } from '../constants';
import { fromSecretSummaries, fromSecretTable } from '../utils/parsers';

type ReleaseSecrets = {
  secrets: KubeObject[] | undefined;
  state: LoadState;
  readAt: number;
};

const metadataTable = {
  queryParams: { fieldSelector: releaseSecretSelector },
  headers: { Accept: metadataTableAccept },
};

export const useReleaseSecrets = (): ReleaseSecrets => {
  const hasSummaries = hasFeature(useServerConfig(), 'secretSummaries');
  const polled = { ...polledWhen(useContext(LiveRefreshContext)), meta: releaseReadMeta };
  const summaries = useListSecretSummaries(hasSummaries ? {} : skipToken, polled);
  const table = useListCoreV1SecretForAllNamespaces(
    hasSummaries ? skipToken : metadataTable,
    polled,
  );
  const read = hasSummaries ? summaries : table;
  const answered = useMemo(
    () => (hasSummaries ? fromSecretSummaries(summaries.data) : fromSecretTable(table.data)),
    [hasSummaries, summaries.data, table.data],
  );
  const [lastReadable, setLastReadable] = useState(answered);

  if (answered !== undefined && answered !== lastReadable) {
    setLastReadable(answered);
  }

  return {
    secrets: answered ?? lastReadable,
    state: read.data !== undefined && answered === undefined ? 'failed' : toLoadState(read),
    readAt: read.dataUpdatedAt,
  };
};
