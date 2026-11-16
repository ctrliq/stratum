import {
  useCreateNetworkingV1NamespacedIngress,
  useDeleteNetworkingV1NamespacedIngress,
  useListNetworkingV1IngressForAllNamespaces,
  useReplaceNetworkingV1NamespacedIngress,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { ingressColumns, ingressKind } from '../../constants';
import { toIngressRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function IngressesList({ linkedName }: Props) {
  const list = useListNetworkingV1IngressForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateNetworkingV1NamespacedIngress();
  const { mutateAsync: replaced } = useReplaceNetworkingV1NamespacedIngress();
  const { mutateAsync: deleted } = useDeleteNetworkingV1NamespacedIngress();

  return (
    <ResourceBrowser
      kind={ingressKind}
      list={list}
      columns={ingressColumns}
      toRow={toIngressRow}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
