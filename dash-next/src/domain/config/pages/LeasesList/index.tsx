import {
  useCreateCoordinationV1NamespacedLease,
  useDeleteCoordinationV1NamespacedLease,
  useListCoordinationV1LeaseForAllNamespaces,
  useReplaceCoordinationV1NamespacedLease,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { leaseColumns, leaseKind } from '../../constants';
import { toLeaseRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function LeasesList({ linkedName }: Props) {
  const list = useListCoordinationV1LeaseForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateCoordinationV1NamespacedLease();
  const { mutateAsync: replaced } = useReplaceCoordinationV1NamespacedLease();
  const { mutateAsync: deleted } = useDeleteCoordinationV1NamespacedLease();

  return (
    <ResourceBrowser
      kind={leaseKind}
      list={list}
      columns={leaseColumns}
      toRow={toLeaseRow}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
