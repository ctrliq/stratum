import {
  useCreateCoreV1NamespacedPersistentVolumeClaim,
  useDeleteCoreV1NamespacedPersistentVolumeClaim,
  useListCoreV1PersistentVolumeClaimForAllNamespaces,
  useReplaceCoreV1NamespacedPersistentVolumeClaim,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { claimColumns, claimKind } from '../../constants';
import { toClaimRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function PersistentVolumeClaimsList({ linkedName }: Props) {
  const list = useListCoreV1PersistentVolumeClaimForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateCoreV1NamespacedPersistentVolumeClaim();
  const { mutateAsync: replaced } = useReplaceCoreV1NamespacedPersistentVolumeClaim();
  const { mutateAsync: deleted } = useDeleteCoreV1NamespacedPersistentVolumeClaim();

  return (
    <ResourceBrowser
      kind={claimKind}
      list={list}
      columns={claimColumns}
      toRow={toClaimRow}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
