import { LucideIconList } from '@ctrliq/quantic-react';
import { useContext } from 'react';

import {
  useCreateApiextensionsV1CustomResourceDefinition,
  useDeleteApiextensionsV1CustomResourceDefinition,
  useListApiextensionsV1CustomResourceDefinition,
  useReplaceApiextensionsV1CustomResourceDefinition,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';
import { useScreenNavigation } from '~/domain/shell/hooks/useScreenNavigation';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { NamespaceContext } from '~/shared/context/NamespaceContext';

import { browseInstances, definitionColumns, definitionKind } from '../../constants';
import { toInstancesHref } from '../../utils/formatters';
import { addressOf } from '../../utils/selectors';
import { toDefinitionRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function CrdsList({ linkedName }: Props) {
  const clusterId = useContext(ClusterContext);
  const namespace = useContext(NamespaceContext);
  const { onOpened } = useScreenNavigation();
  const list = useListApiextensionsV1CustomResourceDefinition({}, resourceListQuery);
  const { mutateAsync: created } = useCreateApiextensionsV1CustomResourceDefinition();
  const { mutateAsync: replaced } = useReplaceApiextensionsV1CustomResourceDefinition();
  const { mutateAsync: deleted } = useDeleteApiextensionsV1CustomResourceDefinition();

  return (
    <ResourceBrowser
      kind={definitionKind}
      list={list}
      columns={definitionColumns}
      toRow={toDefinitionRow}
      linkedName={linkedName}
      actions={[
        {
          title: browseInstances,
          icon: <LucideIconList size="sm" />,
          isWrite: false,
          onRun: (definition) => {
            onOpened(toInstancesHref(clusterId ?? '', addressOf(definition), namespace));
          },
        },
      ]}
      writes={{
        onCreate: (_namespace, body) => created({ body }),
        onReplace: ({ name }, body) => replaced({ pathParams: { name }, body }),
        onDelete: ({ name }) => deleted({ pathParams: { name } }),
      }}
    />
  );
}
