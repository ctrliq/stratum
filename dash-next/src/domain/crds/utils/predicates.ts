import type {
  CustomResourceColumnDefinition,
  CustomResourceDefinition,
} from '~/api/k8s/k8sSchemas';
import { isDashboardServer } from '~/domain/auth/utils/predicates';
import type { ResourceKind } from '~/domain/resources/types/resources';
import type { ServerConfig } from '~/shared/context/ConfigContext';

import { hubManagedPlurals, identityPaths, stratumGroup } from '../constants';
import type { InstanceAddress, PathStep } from '../types/crds';

export const isIdentityPath = (steps: PathStep[] | undefined): boolean =>
  steps !== undefined &&
  identityPaths.includes(steps.map((step) => (step.type === 'field' ? step.name : '[]')).join('.'));

export const isShownColumn = ({
  name,
  jsonPath,
  priority = 0,
}: Partial<CustomResourceColumnDefinition>): boolean =>
  Boolean(name) && Boolean(jsonPath) && priority <= 0;

export const isHubManaged = (
  config: ServerConfig,
  { group, plural }: Pick<InstanceAddress, 'group' | 'plural'>,
): boolean =>
  !isDashboardServer(config) && group === stratumGroup && hubManagedPlurals.includes(plural);

export const hasUnresolvedNamespace = (
  definition: CustomResourceDefinition | undefined,
  { isNamespaced }: Pick<ResourceKind, 'isNamespaced'>,
): boolean => definition === undefined && isNamespaced;
