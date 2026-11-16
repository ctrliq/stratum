import {
  useCreateCoreV1NamespacedService,
  useDeleteCoreV1NamespacedService,
  useListCoreV1ServiceForAllNamespaces,
  useReplaceCoreV1NamespacedService,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { serviceColumns, serviceKind } from '../../constants';
import { toServiceRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function ServicesList({ linkedName }: Props) {
  const list = useListCoreV1ServiceForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateCoreV1NamespacedService();
  const { mutateAsync: replaced } = useReplaceCoreV1NamespacedService();
  const { mutateAsync: deleted } = useDeleteCoreV1NamespacedService();

  return (
    <ResourceBrowser
      kind={serviceKind}
      list={list}
      columns={serviceColumns}
      toRow={toServiceRow}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
