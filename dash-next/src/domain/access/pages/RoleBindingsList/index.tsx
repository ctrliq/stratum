import {
  useCreateRbacAuthorizationV1NamespacedRoleBinding,
  useDeleteRbacAuthorizationV1NamespacedRoleBinding,
  useListRbacAuthorizationV1RoleBindingForAllNamespaces,
  useReplaceRbacAuthorizationV1NamespacedRoleBinding,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { bindingColumns, roleBindingKind } from '../../constants';
import { toBindingRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function RoleBindingsList({ linkedName }: Props) {
  const list = useListRbacAuthorizationV1RoleBindingForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateRbacAuthorizationV1NamespacedRoleBinding();
  const { mutateAsync: replaced } = useReplaceRbacAuthorizationV1NamespacedRoleBinding();
  const { mutateAsync: deleted } = useDeleteRbacAuthorizationV1NamespacedRoleBinding();

  return (
    <ResourceBrowser
      kind={roleBindingKind}
      list={list}
      columns={bindingColumns}
      toRow={toBindingRow}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
