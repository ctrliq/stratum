import {
  useCreateCoreV1NamespacedResourceQuota,
  useDeleteCoreV1NamespacedResourceQuota,
  useListCoreV1ResourceQuotaForAllNamespaces,
  useReplaceCoreV1NamespacedResourceQuota,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { resourceQuotaColumns, resourceQuotaKind } from '../../constants';
import { toResourceQuotaRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function ResourceQuotasList({ linkedName }: Props) {
  const list = useListCoreV1ResourceQuotaForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateCoreV1NamespacedResourceQuota();
  const { mutateAsync: replaced } = useReplaceCoreV1NamespacedResourceQuota();
  const { mutateAsync: deleted } = useDeleteCoreV1NamespacedResourceQuota();

  return (
    <ResourceBrowser
      kind={resourceQuotaKind}
      list={list}
      columns={resourceQuotaColumns}
      toRow={toResourceQuotaRow}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
