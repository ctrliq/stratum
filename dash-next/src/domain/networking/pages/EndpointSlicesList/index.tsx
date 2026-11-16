import {
  useCreateDiscoveryV1NamespacedEndpointSlice,
  useDeleteDiscoveryV1NamespacedEndpointSlice,
  useListDiscoveryV1EndpointSliceForAllNamespaces,
  useReplaceDiscoveryV1NamespacedEndpointSlice,
} from '~/api/k8s/k8sComponents';
import { isDashboardServer } from '~/domain/auth/utils/predicates';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';
import { useServerConfig } from '~/shared/context/ConfigContext';

import { endpointSliceColumns, endpointSliceKind } from '../../constants';
import { toEndpointSliceRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function EndpointSlicesList({ linkedName }: Props) {
  const config = useServerConfig();
  const list = useListDiscoveryV1EndpointSliceForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateDiscoveryV1NamespacedEndpointSlice();
  const { mutateAsync: replaced } = useReplaceDiscoveryV1NamespacedEndpointSlice();
  const { mutateAsync: deleted } = useDeleteDiscoveryV1NamespacedEndpointSlice();

  return (
    <ResourceBrowser
      kind={endpointSliceKind}
      list={list}
      columns={endpointSliceColumns}
      toRow={toEndpointSliceRow}
      linkedName={linkedName}
      isReadOnly={!isDashboardServer(config)}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
