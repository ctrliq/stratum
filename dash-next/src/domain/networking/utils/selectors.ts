import type { Endpoints, Ingress } from '~/api/k8s/k8sSchemas';

export const readyAddressesOf = ({ subsets }: Pick<Endpoints, 'subsets'>): string[] =>
  (subsets ?? []).flatMap(({ addresses }) => (addresses ?? []).map(({ ip }) => ip));

export const hostsOf = ({ spec }: Pick<Ingress, 'spec'>): string[] =>
  (spec?.rules ?? []).flatMap(({ host }) => (host ? [host] : []));
