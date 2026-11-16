import {
  useCreateBatchV1NamespacedJob,
  useDeleteBatchV1NamespacedJob,
  useListBatchV1JobForAllNamespaces,
  useReplaceBatchV1NamespacedJob,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { jobColumns, jobKind } from '../../constants';
import { toJobCells } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function JobsList({ linkedName }: Props) {
  const list = useListBatchV1JobForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateBatchV1NamespacedJob();
  const { mutateAsync: replaced } = useReplaceBatchV1NamespacedJob();
  const { mutateAsync: deleted } = useDeleteBatchV1NamespacedJob();

  return (
    <ResourceBrowser
      kind={jobKind}
      list={list}
      columns={jobColumns}
      toRow={toJobCells}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
