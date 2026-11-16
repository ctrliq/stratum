import {
  useCreateAppsV1NamespacedDaemonSet,
  useDeleteAppsV1NamespacedDaemonSet,
  useListAppsV1DaemonSetForAllNamespaces,
  usePatchAppsV1NamespacedDaemonSet,
  useReplaceAppsV1NamespacedDaemonSet,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { daemonSetColumns, daemonSetKind } from '../../constants';
import { toDaemonSetCells, toRestartPatch } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function DaemonSetsList({ linkedName }: Props) {
  const list = useListAppsV1DaemonSetForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateAppsV1NamespacedDaemonSet();
  const { mutateAsync: replaced } = useReplaceAppsV1NamespacedDaemonSet();
  const { mutateAsync: deleted } = useDeleteAppsV1NamespacedDaemonSet();
  const { mutateAsync: patched } = usePatchAppsV1NamespacedDaemonSet();

  return (
    <ResourceBrowser
      kind={daemonSetKind}
      list={list}
      columns={daemonSetColumns}
      toRow={toDaemonSetCells}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
        onRestart: (pathParams) => patched({ pathParams, body: toRestartPatch(new Date()) }),
      }}
    />
  );
}
