import {
  useCreateAppsV1NamespacedStatefulSet,
  useDeleteAppsV1NamespacedStatefulSet,
  useListAppsV1StatefulSetForAllNamespaces,
  usePatchAppsV1NamespacedStatefulSet,
  usePatchAppsV1NamespacedStatefulSetScale,
  useReplaceAppsV1NamespacedStatefulSet,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { statefulSetColumns, statefulSetKind } from '../../constants';
import { toRestartPatch, toScalePatch, toStatefulSetCells } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function StatefulSetsList({ linkedName }: Props) {
  const list = useListAppsV1StatefulSetForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateAppsV1NamespacedStatefulSet();
  const { mutateAsync: replaced } = useReplaceAppsV1NamespacedStatefulSet();
  const { mutateAsync: deleted } = useDeleteAppsV1NamespacedStatefulSet();
  const { mutateAsync: patched } = usePatchAppsV1NamespacedStatefulSet();
  const { mutateAsync: scaled } = usePatchAppsV1NamespacedStatefulSetScale();

  return (
    <ResourceBrowser
      kind={statefulSetKind}
      list={list}
      columns={statefulSetColumns}
      toRow={toStatefulSetCells}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
        onRestart: (pathParams) => patched({ pathParams, body: toRestartPatch(new Date()) }),
        scale: {
          replicasOf: ({ spec }) => spec.replicas,
          onScale: (pathParams, replicas) => scaled({ pathParams, body: toScalePatch(replicas) }),
        },
      }}
    />
  );
}
