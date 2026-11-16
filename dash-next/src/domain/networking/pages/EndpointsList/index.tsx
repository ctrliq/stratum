import {
  useCreateCoreV1NamespacedEndpoints,
  useDeleteCoreV1NamespacedEndpoints,
  useListCoreV1EndpointsForAllNamespaces,
  useReplaceCoreV1NamespacedEndpoints,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { endpointsColumns, endpointsKind } from '../../constants';
import { toEndpointsRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function EndpointsList({ linkedName }: Props) {
  const list = useListCoreV1EndpointsForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateCoreV1NamespacedEndpoints();
  const { mutateAsync: replaced } = useReplaceCoreV1NamespacedEndpoints();
  const { mutateAsync: deleted } = useDeleteCoreV1NamespacedEndpoints();

  return (
    <ResourceBrowser
      kind={endpointsKind}
      list={list}
      columns={endpointsColumns}
      toRow={toEndpointsRow}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
