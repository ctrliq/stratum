import { IconSize } from '@ctrliq/quantic-components';
import { LucideIconExternalLink, SidebarMenuItem } from '@ctrliq/quantic-react';

import { isPlainClick } from '~/shared/utils/predicates';

import { useScreenNavigation } from '../../hooks/useScreenNavigation';
import type { SidebarRow } from '../../types/screen';

type Props = {
  row: SidebarRow;
  isSelected: boolean;
};

const separateTab = 'noopener,noreferrer';

export const SidebarLink = ({ row, isSelected }: Props) => {
  const { onLinkClicked } = useScreenNavigation();
  return (
    <SidebarMenuItem
      href={row.href}
      label={row.label}
      selected={isSelected}
      onClick={(event) => {
        if (row.isExternal && isPlainClick(event)) {
          event.preventDefault();
          window.open(row.href, '_blank', separateTab);
        } else if (!row.isExternal) {
          onLinkClicked(event, row.href);
        }
      }}
    >
      {row.icon ? <row.icon slot="icon" size={IconSize.Small} /> : null}
      {row.isExternal ? (
        <LucideIconExternalLink
          slot="after"
          size={IconSize.ExtraSmall}
          aria-label="Opens in a new tab"
        />
      ) : row.count === undefined ? null : (
        <span slot="after">{row.count}</span>
      )}
    </SidebarMenuItem>
  );
};
