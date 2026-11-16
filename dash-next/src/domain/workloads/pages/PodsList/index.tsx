import { LucideIconScrollText, LucideIconTerminal } from '@ctrliq/quantic-react';

import {
  useCreateCoreV1NamespacedPod,
  useDeleteCoreV1NamespacedPod,
  useListCoreV1PodForAllNamespaces,
  useReplaceCoreV1NamespacedPod,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';
import { useTerminal } from '~/domain/terminal/hooks/useTerminal';

import { podColumns, podKind } from '../../constants';
import { targetOfPod } from '../../utils/selectors';
import { toPodCells } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function PodsList({ linkedName }: Props) {
  const { onLogsOpened, onExecOpened } = useTerminal();
  const list = useListCoreV1PodForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateCoreV1NamespacedPod();
  const { mutateAsync: replaced } = useReplaceCoreV1NamespacedPod();
  const { mutateAsync: deleted } = useDeleteCoreV1NamespacedPod();

  return (
    <ResourceBrowser
      kind={podKind}
      list={list}
      columns={podColumns}
      toRow={toPodCells}
      linkedName={linkedName}
      actions={[
        {
          title: 'Logs',
          icon: <LucideIconScrollText size="sm" />,
          isWrite: false,
          onRun: (pod) => onLogsOpened(targetOfPod(pod)),
        },
        {
          title: 'Exec',
          icon: <LucideIconTerminal size="sm" />,
          isWrite: true,
          onRun: (pod) => onExecOpened(targetOfPod(pod)),
        },
      ]}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
