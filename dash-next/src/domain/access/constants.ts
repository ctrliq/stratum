import { CellAlign } from '@ctrliq/quantic-components';

import type {
  ClusterRole,
  ClusterRoleBinding,
  Role,
  RoleBinding,
  ServiceAccount,
} from '~/api/k8s/k8sSchemas';
import type { ResourceColumns, ResourceKind } from '~/domain/resources/types/resources';

import type { BindingCells, RulesCells, ServiceAccountCells } from './types/access';

const rbacVersion = 'rbac.authorization.k8s.io/v1';
const counted = { meta: { align: CellAlign.Right } };

export const serviceAccountKind: ResourceKind = {
  apiVersion: 'v1',
  kind: 'ServiceAccount',
  plural: 'Service Accounts',
  isNamespaced: true,
};

export const roleKind: ResourceKind = {
  apiVersion: rbacVersion,
  kind: 'Role',
  plural: 'Roles',
  isNamespaced: true,
};

export const roleBindingKind: ResourceKind = {
  apiVersion: rbacVersion,
  kind: 'RoleBinding',
  plural: 'Role Bindings',
  isNamespaced: true,
};

export const clusterRoleKind: ResourceKind = {
  apiVersion: rbacVersion,
  kind: 'ClusterRole',
  plural: 'Cluster Roles',
  isNamespaced: false,
};

export const clusterRoleBindingKind: ResourceKind = {
  apiVersion: rbacVersion,
  kind: 'ClusterRoleBinding',
  plural: 'Cluster Role Bindings',
  isNamespaced: false,
};

export const serviceAccountColumns: ResourceColumns<ServiceAccount, ServiceAccountCells> = [
  { id: 'secrets', header: 'Secrets', accessorKey: 'secrets', ...counted },
];

export const rulesColumns: ResourceColumns<Role | ClusterRole, RulesCells> = [
  { id: 'rules', header: 'Rules', accessorKey: 'rules', ...counted },
];

export const bindingColumns: ResourceColumns<RoleBinding | ClusterRoleBinding, BindingCells> = [
  { id: 'role', header: 'Role', accessorKey: 'role' },
];
