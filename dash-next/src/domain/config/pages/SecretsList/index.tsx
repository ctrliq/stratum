import { skipToken } from '@tanstack/react-query';

import { useListSecretSummaries } from '~/api/dashboard/dashboardComponents';
import {
  readCoreV1NamespacedSecretQuery,
  useCreateCoreV1NamespacedSecret,
  useDeleteCoreV1NamespacedSecret,
  useListCoreV1SecretForAllNamespaces,
  useReplaceCoreV1NamespacedSecret,
} from '~/api/k8s/k8sComponents';
import { useK8sContext } from '~/api/k8s/k8sContext';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { hasFeature } from '~/shared/utils/predicates';

import { secretColumns, secretKind, tableAccept } from '../../constants';
import { fromSecretTable } from '../../utils/parsers';
import { toSecretListings, toSecretRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

const asTable = { headers: { Accept: tableAccept } };

export function SecretsList({ linkedName }: Props) {
  const hasSummaries = hasFeature(useServerConfig(), 'secretSummaries');
  const { fetcherOptions } = useK8sContext();
  const summaries = useListSecretSummaries(hasSummaries ? {} : skipToken, {
    ...resourceListQuery,
    select: toSecretListings,
  });
  const table = useListCoreV1SecretForAllNamespaces(hasSummaries ? skipToken : asTable, {
    ...resourceListQuery,
    select: fromSecretTable,
  });
  const { mutateAsync: created } = useCreateCoreV1NamespacedSecret();
  const { mutateAsync: replaced } = useReplaceCoreV1NamespacedSecret();
  const { mutateAsync: deleted } = useDeleteCoreV1NamespacedSecret();

  return (
    <ResourceBrowser
      kind={secretKind}
      list={hasSummaries ? summaries : table}
      columns={secretColumns}
      toRow={toSecretRow}
      linkedName={linkedName}
      openedQuery={(pathParams) =>
        readCoreV1NamespacedSecretQuery({ ...fetcherOptions, pathParams })
      }
      writes={{
        onCreate: (namespace, body) => created({ pathParams: { namespace }, body }),
        onReplace: (pathParams, body) => replaced({ pathParams, body }),
        onDelete: (pathParams) => deleted({ pathParams }),
      }}
    />
  );
}
