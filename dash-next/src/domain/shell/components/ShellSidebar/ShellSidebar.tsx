import { Sidebar, SidebarMenuItemGroup } from '@ctrliq/quantic-react';

import { useServerConfig } from '~/shared/context/ConfigContext';

import { crdGroupPrefix } from '../../constants';
import { useClusterIdentity } from '../../hooks/useClusterIdentity';
import { useCollapsedSections } from '../../hooks/useCollapsedSections';
import { useCrdGroups } from '../../hooks/useCrdGroups';
import { useScreenPlace } from '../../hooks/useScreenPlace';
import { useSidebarCounts } from '../../hooks/useSidebarCounts';
import type { Fleet } from '../../types/screen';
import { isCrdGroupOpen, isFleetMode, isSectionCollapsed } from '../../utils/predicates';
import { selectedRowOf } from '../../utils/selectors';
import { toSidebarSections } from '../../utils/transforms';
import { ClusterLabel, ClusterSwitcher } from '../ClusterBlock/ClusterBlock';
import { SidebarFold } from './SidebarFold';
import { SidebarLink } from './SidebarLink';

type Props = {
  fleet: Fleet;
};

export const ShellSidebar = ({ fleet }: Props) => {
  const config = useServerConfig();
  const { clusterId, namespace, location, tab } = useScreenPlace();
  const { collapsed, onToggled } = useCollapsedSections();
  const counts = useSidebarCounts(fleet);
  const crdGroups = useCrdGroups();
  const selected = selectedRowOf(location, tab);
  const cluster = useClusterIdentity(fleet);
  const sections = toSidebarSections({
    config,
    clusterId,
    namespace,
    counts,
    crdGroups,
  });

  return (
    <Sidebar slot="sidebar" collapsible aria-label="Screens">
      {isFleetMode(config) ? (
        <ClusterSwitcher cluster={cluster} />
      ) : (
        <ClusterLabel cluster={cluster} />
      )}
      {sections.map((section) => {
        const isCollapsed = isSectionCollapsed(collapsed, section.id);
        return (
          <SidebarMenuItemGroup key={section.id}>
            {section.label ? (
              <SidebarFold
                label={section.label}
                isOpen={!isCollapsed}
                onToggled={() => {
                  onToggled(section.id, isCollapsed);
                }}
              />
            ) : null}
            {isCollapsed
              ? null
              : section.rows.map((row) => (
                  <SidebarLink key={row.id} row={row} isSelected={row.id === selected} />
                ))}
            {isCollapsed
              ? null
              : section.groups.map((group) => {
                  const isOpen = isCrdGroupOpen(
                    collapsed,
                    group.id,
                    group.rows.some(({ id }) => id === selected),
                  );
                  return [
                    <SidebarFold
                      key={group.id}
                      label={group.label}
                      count={group.rows.length}
                      isOpen={isOpen}
                      onToggled={() => {
                        onToggled(`${crdGroupPrefix}${group.id}`, !isOpen);
                      }}
                    />,
                    ...(isOpen
                      ? group.rows.map((row) => (
                          <SidebarLink key={row.id} row={row} isSelected={row.id === selected} />
                        ))
                      : []),
                  ];
                })}
          </SidebarMenuItemGroup>
        );
      })}
    </Sidebar>
  );
};
