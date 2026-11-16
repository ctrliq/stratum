import {
  useCreateStratumCiqComV1NamespacedHelmChart,
  useDeleteStratumCiqComV1NamespacedHelmChart,
  useListStratumCiqComV1HelmChartForAllNamespaces,
  useReplaceStratumCiqComV1NamespacedHelmChart,
} from '~/api/k8s/k8sComponents';
import { isDashboardServer } from '~/domain/auth/utils/predicates';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { toStatusLabel, toStatusTone } from '~/shared/utils/formatters';

import { helmChartKind } from '../../constants';
import type { HelmChartColumns } from '../../types/charts';
import { toHelmChartRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

const columns: HelmChartColumns = [
  { id: 'chart', header: 'Chart', accessorKey: 'chart' },
  { id: 'version', header: 'Version', accessorKey: 'version' },
  { id: 'targetNamespace', header: 'Target NS', accessorKey: 'targetNamespace' },
  {
    id: 'phase',
    header: 'Phase',
    cell: ({ row }) => (
      <ToneBadge tone={toStatusTone(row.original.phase)}>
        {toStatusLabel(row.original.phase)}
      </ToneBadge>
    ),
  },
];

export function HelmChartsList({ linkedName }: Props) {
  const isReadOnly = !isDashboardServer(useServerConfig());
  const list = useListStratumCiqComV1HelmChartForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateStratumCiqComV1NamespacedHelmChart();
  const { mutateAsync: replaced } = useReplaceStratumCiqComV1NamespacedHelmChart();
  const { mutateAsync: deleted } = useDeleteStratumCiqComV1NamespacedHelmChart();

  return (
    <ResourceBrowser
      kind={helmChartKind}
      list={list}
      columns={columns}
      toRow={toHelmChartRow}
      linkedName={linkedName}
      isReadOnly={isReadOnly}
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
