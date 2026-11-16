import type { Role, RoleBinding, ServiceAccount } from '~/api/k8s/k8sSchemas';
import { toDisplayText } from '~/shared/utils/formatters';

import type { BindingCells, RulesCells, ServiceAccountCells } from '../types/access';

export const toServiceAccountRow = ({ secrets }: ServiceAccount): ServiceAccountCells => ({
  secrets: (secrets ?? []).length,
});

export const toRulesRow = ({ rules }: Pick<Role, 'rules'>): RulesCells => ({
  rules: (rules ?? []).length,
});

export const toBindingRow = ({ roleRef }: Partial<Pick<RoleBinding, 'roleRef'>>): BindingCells => ({
  role: toDisplayText(roleRef?.name),
});
