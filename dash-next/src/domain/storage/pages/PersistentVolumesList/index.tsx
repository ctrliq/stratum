import {
  useCreateCoreV1PersistentVolume,
  useDeleteCoreV1PersistentVolume,
  useListCoreV1PersistentVolume,
  useReplaceCoreV1PersistentVolume,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { volumeColumns, volumeKind } from '../../constants';
import { toVolumeRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function PersistentVolumesList({ linkedName }: Props) {
  const list = useListCoreV1PersistentVolume({}, resourceListQuery);
  const { mutateAsync: created } = useCreateCoreV1PersistentVolume();
  const { mutateAsync: replaced } = useReplaceCoreV1PersistentVolume();
  const { mutateAsync: deleted } = useDeleteCoreV1PersistentVolume();

  return (
    <ResourceBrowser
      kind={volumeKind}
      list={list}
      columns={volumeColumns}
      toRow={toVolumeRow}
      linkedName={linkedName}
      writes={{
        onCreate: (_namespace, body) => created({ body }),
        onReplace: ({ name }, body) => replaced({ pathParams: { name }, body }),
        onDelete: ({ name }) => deleted({ pathParams: { name } }),
      }}
    />
  );
}
