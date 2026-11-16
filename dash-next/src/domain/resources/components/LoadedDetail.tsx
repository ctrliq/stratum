import { useQuery, type UseQueryOptions } from '@tanstack/react-query';

import type { KubeObject } from '~/shared/types/kubernetes';
import { toErrorNotice } from '~/shared/utils/formatters';
import { nameOf } from '~/shared/utils/selectors';

import { loadingSummary } from '../constants';
import { ResourceDetail, type ResourceDetailProps } from './ResourceDetail';
import { ResourcePanel } from './ResourcePanel';

type Props<TOpened extends KubeObject> = Omit<ResourceDetailProps, 'object'> & {
  listed: KubeObject;
  query: Pick<UseQueryOptions<TOpened>, 'queryKey' | 'queryFn'>;
};

const readOnOpening = { gcTime: 0, refetchOnWindowFocus: false, refetchOnReconnect: false };

export function LoadedDetail<TOpened extends KubeObject>({
  listed,
  query,
  ...detail
}: Props<TOpened>) {
  const opened = useQuery({ ...query, ...readOnOpening });

  return opened.data ? (
    <ResourceDetail object={opened.data} {...detail} />
  ) : (
    <ResourcePanel
      title={nameOf(listed)}
      subtitle={detail.kind.kind}
      onClosed={detail.onClosed}
      footer={null}
    >
      <p role="status">{opened.error ? toErrorNotice(opened.error) : loadingSummary}</p>
    </ResourcePanel>
  );
}
