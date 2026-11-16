import {
  useCreateStorageV1StorageClass,
  useDeleteStorageV1StorageClass,
  useListStorageV1StorageClass,
  useReplaceStorageV1StorageClass,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { storageClassColumns, storageClassKind } from '../../constants';
import { toStorageClassRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function StorageClassesList({ linkedName }: Props) {
  const list = useListStorageV1StorageClass({}, resourceListQuery);
  const { mutateAsync: created } = useCreateStorageV1StorageClass();
  const { mutateAsync: replaced } = useReplaceStorageV1StorageClass();
  const { mutateAsync: deleted } = useDeleteStorageV1StorageClass();

  return (
    <ResourceBrowser
      kind={storageClassKind}
      list={list}
      columns={storageClassColumns}
      toRow={toStorageClassRow}
      linkedName={linkedName}
      writes={{
        onCreate: (_namespace, body) => created({ body }),
        onReplace: ({ name }, body) => replaced({ pathParams: { name }, body }),
        onDelete: ({ name }) => deleted({ pathParams: { name } }),
      }}
    />
  );
}
