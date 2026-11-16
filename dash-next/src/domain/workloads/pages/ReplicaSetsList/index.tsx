import {
  useCreateAppsV1NamespacedReplicaSet,
  useDeleteAppsV1NamespacedReplicaSet,
  useListAppsV1ReplicaSetForAllNamespaces,
  usePatchAppsV1NamespacedReplicaSetScale,
  useReplaceAppsV1NamespacedReplicaSet,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { replicaSetColumns, replicaSetKind } from '../../constants';
import { toReplicaSetCells, toScalePatch } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function ReplicaSetsList({ linkedName }: Props) {
  const list = useListAppsV1ReplicaSetForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateAppsV1NamespacedReplicaSet();
  const { mutateAsync: replaced } = useReplaceAppsV1NamespacedReplicaSet();
  const { mutateAsync: deleted } = useDeleteAppsV1NamespacedReplicaSet();
  const { mutateAsync: scaled } = usePatchAppsV1NamespacedReplicaSetScale();

  return (
    <ResourceBrowser
      kind={replicaSetKind}
      list={list}
      columns={replicaSetColumns}
      toRow={toReplicaSetCells}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
        scale: {
          replicasOf: ({ spec }) => spec.replicas,
          onScale: (pathParams, replicas) => scaled({ pathParams, body: toScalePatch(replicas) }),
        },
      }}
    />
  );
}
