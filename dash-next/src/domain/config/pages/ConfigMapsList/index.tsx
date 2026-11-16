import {
  useCreateCoreV1NamespacedConfigMap,
  useDeleteCoreV1NamespacedConfigMap,
  useListCoreV1ConfigMapForAllNamespaces,
  useReplaceCoreV1NamespacedConfigMap,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { configMapColumns, configMapKind } from '../../constants';
import { toConfigMapRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function ConfigMapsList({ linkedName }: Props) {
  const list = useListCoreV1ConfigMapForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateCoreV1NamespacedConfigMap();
  const { mutateAsync: replaced } = useReplaceCoreV1NamespacedConfigMap();
  const { mutateAsync: deleted } = useDeleteCoreV1NamespacedConfigMap();

  return (
    <ResourceBrowser
      kind={configMapKind}
      list={list}
      columns={configMapColumns}
      toRow={toConfigMapRow}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
