import {
  useCreateRbacAuthorizationV1NamespacedRole,
  useDeleteRbacAuthorizationV1NamespacedRole,
  useListRbacAuthorizationV1RoleForAllNamespaces,
  useReplaceRbacAuthorizationV1NamespacedRole,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { roleKind, rulesColumns } from '../../constants';
import { toRulesRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function RolesList({ linkedName }: Props) {
  const list = useListRbacAuthorizationV1RoleForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateRbacAuthorizationV1NamespacedRole();
  const { mutateAsync: replaced } = useReplaceRbacAuthorizationV1NamespacedRole();
  const { mutateAsync: deleted } = useDeleteRbacAuthorizationV1NamespacedRole();

  return (
    <ResourceBrowser
      kind={roleKind}
      list={list}
      columns={rulesColumns}
      toRow={toRulesRow}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
