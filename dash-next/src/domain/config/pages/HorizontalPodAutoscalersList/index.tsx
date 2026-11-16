import {
  useCreateAutoscalingV2NamespacedHorizontalPodAutoscaler,
  useDeleteAutoscalingV2NamespacedHorizontalPodAutoscaler,
  useListAutoscalingV2HorizontalPodAutoscalerForAllNamespaces,
  useReplaceAutoscalingV2NamespacedHorizontalPodAutoscaler,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { autoscalerColumns, autoscalerKind } from '../../constants';
import { toAutoscalerRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function HorizontalPodAutoscalersList({ linkedName }: Props) {
  const list = useListAutoscalingV2HorizontalPodAutoscalerForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateAutoscalingV2NamespacedHorizontalPodAutoscaler();
  const { mutateAsync: replaced } = useReplaceAutoscalingV2NamespacedHorizontalPodAutoscaler();
  const { mutateAsync: deleted } = useDeleteAutoscalingV2NamespacedHorizontalPodAutoscaler();

  return (
    <ResourceBrowser
      kind={autoscalerKind}
      list={list}
      columns={autoscalerColumns}
      toRow={toAutoscalerRow}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
