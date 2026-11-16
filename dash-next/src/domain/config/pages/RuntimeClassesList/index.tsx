import {
  useCreateNodeV1RuntimeClass,
  useDeleteNodeV1RuntimeClass,
  useListNodeV1RuntimeClass,
  useReplaceNodeV1RuntimeClass,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { runtimeClassColumns, runtimeClassKind } from '../../constants';
import { toRuntimeClassRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function RuntimeClassesList({ linkedName }: Props) {
  const list = useListNodeV1RuntimeClass({}, resourceListQuery);
  const { mutateAsync: created } = useCreateNodeV1RuntimeClass();
  const { mutateAsync: replaced } = useReplaceNodeV1RuntimeClass();
  const { mutateAsync: deleted } = useDeleteNodeV1RuntimeClass();

  return (
    <ResourceBrowser
      kind={runtimeClassKind}
      list={list}
      columns={runtimeClassColumns}
      toRow={toRuntimeClassRow}
      linkedName={linkedName}
      writes={{
        onCreate: (_namespace, body) => created({ body }),
        onReplace: ({ name }, body) => replaced({ pathParams: { name }, body }),
        onDelete: ({ name }) => deleted({ pathParams: { name } }),
      }}
    />
  );
}
