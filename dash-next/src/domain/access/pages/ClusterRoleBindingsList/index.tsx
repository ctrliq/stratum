import {
  useCreateRbacAuthorizationV1ClusterRoleBinding,
  useDeleteRbacAuthorizationV1ClusterRoleBinding,
  useListRbacAuthorizationV1ClusterRoleBinding,
  useReplaceRbacAuthorizationV1ClusterRoleBinding,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { bindingColumns, clusterRoleBindingKind } from '../../constants';
import { toBindingRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function ClusterRoleBindingsList({ linkedName }: Props) {
  const list = useListRbacAuthorizationV1ClusterRoleBinding({}, resourceListQuery);
  const { mutateAsync: created } = useCreateRbacAuthorizationV1ClusterRoleBinding();
  const { mutateAsync: replaced } = useReplaceRbacAuthorizationV1ClusterRoleBinding();
  const { mutateAsync: deleted } = useDeleteRbacAuthorizationV1ClusterRoleBinding();

  return (
    <ResourceBrowser
      kind={clusterRoleBindingKind}
      list={list}
      columns={bindingColumns}
      toRow={toBindingRow}
      linkedName={linkedName}
      writes={{
        onCreate: (_namespace, body) => created({ body }),
        onReplace: ({ name }, body) => replaced({ pathParams: { name }, body }),
        onDelete: ({ name }) => deleted({ pathParams: { name } }),
      }}
    />
  );
}
