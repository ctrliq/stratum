import {
  useCreateAppsV1NamespacedDeployment,
  useDeleteAppsV1NamespacedDeployment,
  useListAppsV1DeploymentForAllNamespaces,
  usePatchAppsV1NamespacedDeployment,
  usePatchAppsV1NamespacedDeploymentScale,
  useReplaceAppsV1NamespacedDeployment,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { deploymentColumns, deploymentKind } from '../../constants';
import { toDeploymentCells, toRestartPatch, toScalePatch } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function DeploymentsList({ linkedName }: Props) {
  const list = useListAppsV1DeploymentForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateAppsV1NamespacedDeployment();
  const { mutateAsync: replaced } = useReplaceAppsV1NamespacedDeployment();
  const { mutateAsync: deleted } = useDeleteAppsV1NamespacedDeployment();
  const { mutateAsync: patched } = usePatchAppsV1NamespacedDeployment();
  const { mutateAsync: scaled } = usePatchAppsV1NamespacedDeploymentScale();

  return (
    <ResourceBrowser
      kind={deploymentKind}
      list={list}
      columns={deploymentColumns}
      toRow={toDeploymentCells}
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
