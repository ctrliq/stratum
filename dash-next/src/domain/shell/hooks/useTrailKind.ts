import { skipToken, useQuery } from '@tanstack/react-query';

import { readApiextensionsV1CustomResourceDefinitionQuery } from '~/api/k8s/k8sComponents';
import { useK8sContext } from '~/api/k8s/k8sContext';
import { toDefinitionName } from '~/domain/crds/utils/formatters';

import type { ScreenLocation } from '../types/screen';
import { isCustomResourceList } from '../utils/predicates';
import { toTrailKind } from '../utils/transforms';

const readWithThePage = {
  staleTime: Infinity,
  retry: false,
  refetchOnWindowFocus: false,
  refetchOnReconnect: false,
};

export const useTrailKind = (location: ScreenLocation): string | undefined => {
  const [, group = '', version = '', plural = ''] = location.segments;
  const isListed = isCustomResourceList(location);
  const { fetcherOptions } = useK8sContext();
  const definitionRead = readApiextensionsV1CustomResourceDefinitionQuery(
    isListed && fetcherOptions.clusterId !== undefined
      ? { ...fetcherOptions, pathParams: { name: toDefinitionName({ group, version, plural }) } }
      : skipToken,
  );
  const { data, isPending } = useQuery({
    ...definitionRead,
    queryKey: [...definitionRead.queryKey, version],
    ...readWithThePage,
  });
  return isListed ? toTrailKind(plural, { kind: data?.spec.names.kind, isPending }) : undefined;
};
