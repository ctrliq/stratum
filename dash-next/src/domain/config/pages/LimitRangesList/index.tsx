import {
  useCreateCoreV1NamespacedLimitRange,
  useDeleteCoreV1NamespacedLimitRange,
  useListCoreV1LimitRangeForAllNamespaces,
  useReplaceCoreV1NamespacedLimitRange,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { limitRangeColumns, limitRangeKind } from '../../constants';
import { toLimitRangeRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function LimitRangesList({ linkedName }: Props) {
  const list = useListCoreV1LimitRangeForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateCoreV1NamespacedLimitRange();
  const { mutateAsync: replaced } = useReplaceCoreV1NamespacedLimitRange();
  const { mutateAsync: deleted } = useDeleteCoreV1NamespacedLimitRange();

  return (
    <ResourceBrowser
      kind={limitRangeKind}
      list={list}
      columns={limitRangeColumns}
      toRow={toLimitRangeRow}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
