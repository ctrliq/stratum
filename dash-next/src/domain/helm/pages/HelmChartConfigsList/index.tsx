import {
  useCreateStratumCiqComV1NamespacedHelmChartConfig,
  useDeleteStratumCiqComV1NamespacedHelmChartConfig,
  useListStratumCiqComV1HelmChartConfigForAllNamespaces,
  useReplaceStratumCiqComV1NamespacedHelmChartConfig,
} from '~/api/k8s/k8sComponents';
import { isDashboardServer } from '~/domain/auth/utils/predicates';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';
import { useServerConfig } from '~/shared/context/ConfigContext';

import { helmChartConfigColumns, helmChartConfigKind } from '../../constants';
import { toHelmChartConfigRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function HelmChartConfigsList({ linkedName }: Props) {
  const isReadOnly = !isDashboardServer(useServerConfig());
  const list = useListStratumCiqComV1HelmChartConfigForAllNamespaces({}, resourceListQuery);
  const { mutateAsync: created } = useCreateStratumCiqComV1NamespacedHelmChartConfig();
  const { mutateAsync: replaced } = useReplaceStratumCiqComV1NamespacedHelmChartConfig();
  const { mutateAsync: deleted } = useDeleteStratumCiqComV1NamespacedHelmChartConfig();

  return (
    <ResourceBrowser
      kind={helmChartConfigKind}
      list={list}
      columns={helmChartConfigColumns}
      toRow={toHelmChartConfigRow}
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
