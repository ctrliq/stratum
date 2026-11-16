import {
  useCreateCoreV1Namespace,
  useDeleteCoreV1Namespace,
  useListCoreV1Namespace,
  useReplaceCoreV1Namespace,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { namespaceColumns, namespaceKind } from '../../constants';
import { toNamespaceRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function NamespacesList({ linkedName }: Props) {
  const list = useListCoreV1Namespace({}, resourceListQuery);
  const { mutateAsync: created } = useCreateCoreV1Namespace();
  const { mutateAsync: replaced } = useReplaceCoreV1Namespace();
  const { mutateAsync: deleted } = useDeleteCoreV1Namespace();

  return (
    <ResourceBrowser
      kind={namespaceKind}
      list={list}
      columns={namespaceColumns}
      toRow={toNamespaceRow}
      linkedName={linkedName}
      writes={{
        onCreate: (_namespace, body) => created({ body }),
        onReplace: ({ name }, body) => replaced({ pathParams: { name }, body }),
        onDelete: ({ name }) => deleted({ pathParams: { name } }),
      }}
    />
  );
}
