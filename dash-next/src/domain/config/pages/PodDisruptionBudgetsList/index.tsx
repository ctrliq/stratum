import {
  useCreatePolicyV1NamespacedPodDisruptionBudget,
  useDeletePolicyV1NamespacedPodDisruptionBudget,
  useListPolicyV1PodDisruptionBudgetForAllNamespaces,
  useReplacePolicyV1NamespacedPodDisruptionBudget,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { disruptionBudgetColumns, disruptionBudgetKind } from '../../constants';
import { toDisruptionBudgetRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function PodDisruptionBudgetsList({ linkedName }: Props) {
  const list = useListPolicyV1PodDisruptionBudgetForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreatePolicyV1NamespacedPodDisruptionBudget();
  const { mutateAsync: replaced } = useReplacePolicyV1NamespacedPodDisruptionBudget();
  const { mutateAsync: deleted } = useDeletePolicyV1NamespacedPodDisruptionBudget();

  return (
    <ResourceBrowser
      kind={disruptionBudgetKind}
      list={list}
      columns={disruptionBudgetColumns}
      toRow={toDisruptionBudgetRow}
      linkedName={linkedName}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
