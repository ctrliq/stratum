import {
  useCreateBatchV1NamespacedCronJob,
  useDeleteBatchV1NamespacedCronJob,
  useListBatchV1CronJobForAllNamespaces,
  useReplaceBatchV1NamespacedCronJob,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { cronJobColumns, cronJobKind } from '../../constants';
import { toCronJobCells } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function CronJobsList({ linkedName }: Props) {
  const list = useListBatchV1CronJobForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateBatchV1NamespacedCronJob();
  const { mutateAsync: replaced } = useReplaceBatchV1NamespacedCronJob();
  const { mutateAsync: deleted } = useDeleteBatchV1NamespacedCronJob();

  return (
    <ResourceBrowser
      kind={cronJobKind}
      list={list}
      columns={cronJobColumns}
      toRow={toCronJobCells}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
