import {
  useCreateNetworkingV1IngressClass,
  useDeleteNetworkingV1IngressClass,
  useListNetworkingV1IngressClass,
  useReplaceNetworkingV1IngressClass,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { ingressClassColumns, ingressClassKind } from '../../constants';
import { toIngressClassRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function IngressClassesList({ linkedName }: Props) {
  const list = useListNetworkingV1IngressClass({}, resourceListQuery);
  const { mutateAsync: created } = useCreateNetworkingV1IngressClass();
  const { mutateAsync: replaced } = useReplaceNetworkingV1IngressClass();
  const { mutateAsync: deleted } = useDeleteNetworkingV1IngressClass();

  return (
    <ResourceBrowser
      kind={ingressClassKind}
      list={list}
      columns={ingressClassColumns}
      toRow={toIngressClassRow}
      linkedName={linkedName}
      writes={{
        onCreate: (_namespace, body) => created({ body }),
        onReplace: ({ name }, body) => replaced({ pathParams: { name }, body }),
        onDelete: ({ name }) => deleted({ pathParams: { name } }),
      }}
    />
  );
}
