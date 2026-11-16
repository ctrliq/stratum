import {
  useCreateSchedulingV1PriorityClass,
  useDeleteSchedulingV1PriorityClass,
  useListSchedulingV1PriorityClass,
  useReplaceSchedulingV1PriorityClass,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { priorityClassColumns, priorityClassKind } from '../../constants';
import { toPriorityClassRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function PriorityClassesList({ linkedName }: Props) {
  const list = useListSchedulingV1PriorityClass({}, resourceListQuery);
  const { mutateAsync: created } = useCreateSchedulingV1PriorityClass();
  const { mutateAsync: replaced } = useReplaceSchedulingV1PriorityClass();
  const { mutateAsync: deleted } = useDeleteSchedulingV1PriorityClass();

  return (
    <ResourceBrowser
      kind={priorityClassKind}
      list={list}
      columns={priorityClassColumns}
      toRow={toPriorityClassRow}
      linkedName={linkedName}
      writes={{
        onCreate: (_namespace, body) => created({ body }),
        onReplace: ({ name }, body) => replaced({ pathParams: { name }, body }),
        onDelete: ({ name }) => deleted({ pathParams: { name } }),
      }}
    />
  );
}
