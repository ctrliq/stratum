import {
  useCreateCoreV1NamespacedServiceAccount,
  useDeleteCoreV1NamespacedServiceAccount,
  useListCoreV1ServiceAccountForAllNamespaces,
  useReplaceCoreV1NamespacedServiceAccount,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { serviceAccountColumns, serviceAccountKind } from '../../constants';
import { toServiceAccountRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function ServiceAccountsList({ linkedName }: Props) {
  const list = useListCoreV1ServiceAccountForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateCoreV1NamespacedServiceAccount();
  const { mutateAsync: replaced } = useReplaceCoreV1NamespacedServiceAccount();
  const { mutateAsync: deleted } = useDeleteCoreV1NamespacedServiceAccount();

  return (
    <ResourceBrowser
      kind={serviceAccountKind}
      list={list}
      columns={serviceAccountColumns}
      toRow={toServiceAccountRow}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
