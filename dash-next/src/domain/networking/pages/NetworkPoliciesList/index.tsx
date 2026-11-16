import {
  useCreateNetworkingV1NamespacedNetworkPolicy,
  useDeleteNetworkingV1NamespacedNetworkPolicy,
  useListNetworkingV1NetworkPolicyForAllNamespaces,
  useReplaceNetworkingV1NamespacedNetworkPolicy,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { networkPolicyColumns, networkPolicyKind } from '../../constants';
import { toNetworkPolicyRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function NetworkPoliciesList({ linkedName }: Props) {
  const list = useListNetworkingV1NetworkPolicyForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateNetworkingV1NamespacedNetworkPolicy();
  const { mutateAsync: replaced } = useReplaceNetworkingV1NamespacedNetworkPolicy();
  const { mutateAsync: deleted } = useDeleteNetworkingV1NamespacedNetworkPolicy();

  return (
    <ResourceBrowser
      kind={networkPolicyKind}
      list={list}
      columns={networkPolicyColumns}
      toRow={toNetworkPolicyRow}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
