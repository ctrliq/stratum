import { type ListTab, TabbedLists } from '~/shared/components/TabbedLists/TabbedLists';

import {
  clusterRoleBindingKind,
  clusterRoleKind,
  roleBindingKind,
  roleKind,
  serviceAccountKind,
} from '../../constants';
import { ClusterRoleBindingsList } from '../ClusterRoleBindingsList';
import { ClusterRolesList } from '../ClusterRolesList';
import { RoleBindingsList } from '../RoleBindingsList';
import { RolesList } from '../RolesList';
import { ServiceAccountsList } from '../ServiceAccountsList';

type Props = {
  tab?: string;
  linkedName?: string;
  onTabChosen: (tab: string) => void;
};

const tabs: ListTab[] = [
  { id: 'roles', label: roleKind.plural, List: RolesList },
  { id: 'rolebindings', label: roleBindingKind.plural, List: RoleBindingsList },
  { id: 'clusterroles', label: clusterRoleKind.plural, List: ClusterRolesList },
  {
    id: 'clusterrolebindings',
    label: clusterRoleBindingKind.plural,
    List: ClusterRoleBindingsList,
  },
  { id: 'serviceaccounts', label: serviceAccountKind.plural, List: ServiceAccountsList },
];

export function AccessTabs(props: Props) {
  return <TabbedLists title="RBAC" tabs={tabs} {...props} />;
}
