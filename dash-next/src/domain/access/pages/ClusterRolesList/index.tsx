import {
  useCreateRbacAuthorizationV1ClusterRole,
  useDeleteRbacAuthorizationV1ClusterRole,
  useListRbacAuthorizationV1ClusterRole,
  useReplaceRbacAuthorizationV1ClusterRole,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { clusterRoleKind, rulesColumns } from '../../constants';
import { toRulesRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function ClusterRolesList({ linkedName }: Props) {
  const list = useListRbacAuthorizationV1ClusterRole({}, resourceListQuery);
  const { mutateAsync: created } = useCreateRbacAuthorizationV1ClusterRole();
  const { mutateAsync: replaced } = useReplaceRbacAuthorizationV1ClusterRole();
  const { mutateAsync: deleted } = useDeleteRbacAuthorizationV1ClusterRole();

  return (
    <ResourceBrowser
      kind={clusterRoleKind}
      list={list}
      columns={rulesColumns}
      toRow={toRulesRow}
      linkedName={linkedName}
      writes={{
        onCreate: (_namespace, body) => created({ body }),
        onReplace: ({ name }, body) => replaced({ pathParams: { name }, body }),
        onDelete: ({ name }) => deleted({ pathParams: { name } }),
      }}
    />
  );
}
