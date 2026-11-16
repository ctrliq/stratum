import {
  useCreateCoreV1NamespacedReplicationController,
  useDeleteCoreV1NamespacedReplicationController,
  useListCoreV1ReplicationControllerForAllNamespaces,
  usePatchCoreV1NamespacedReplicationControllerScale,
  useReplaceCoreV1NamespacedReplicationController,
} from '~/api/k8s/k8sComponents';
import { isDashboardServer } from '~/domain/auth/utils/predicates';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';
import { useServerConfig } from '~/shared/context/ConfigContext';

import { replicationControllerColumns, replicationControllerKind } from '../../constants';
import { toReplicationControllerCells, toScalePatch } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function ReplicationControllersList({ linkedName }: Props) {
  const isReadOnly = !isDashboardServer(useServerConfig());
  const list = useListCoreV1ReplicationControllerForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateCoreV1NamespacedReplicationController();
  const { mutateAsync: replaced } = useReplaceCoreV1NamespacedReplicationController();
  const { mutateAsync: deleted } = useDeleteCoreV1NamespacedReplicationController();
  const { mutateAsync: scaled } = usePatchCoreV1NamespacedReplicationControllerScale();

  return (
    <ResourceBrowser
      kind={replicationControllerKind}
      list={list}
      columns={replicationControllerColumns}
      toRow={toReplicationControllerCells}
      linkedName={linkedName}
      isReadOnly={isReadOnly}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
        scale: {
          replicasOf: ({ spec }) => spec?.replicas,
          onScale: (pathParams, replicas) => scaled({ pathParams, body: toScalePatch(replicas) }),
        },
      }}
    />
  );
}
