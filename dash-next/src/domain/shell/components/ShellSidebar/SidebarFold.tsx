import { IconSize, SidebarMenuItemVariant } from '@ctrliq/quantic-components';
import {
  LucideIconChevronDown,
  LucideIconChevronRight,
  SidebarMenuItem,
} from '@ctrliq/quantic-react';

type Props = {
  label: string;
  count?: number;
  isOpen: boolean;
  onToggled: () => void;
};

export const SidebarFold = ({ label, count, isOpen, onToggled }: Props) => (
  <SidebarMenuItem
    variant={SidebarMenuItemVariant.Secondary}
    label={label}
    aria-expanded={isOpen}
    title={isOpen ? `Collapse ${label}` : `Expand ${label}`}
    onClick={onToggled}
  >
    {isOpen ? (
      <LucideIconChevronDown slot="icon" size={IconSize.Small} />
    ) : (
      <LucideIconChevronRight slot="icon" size={IconSize.Small} />
    )}
    {count === undefined ? null : <span slot="after">{count}</span>}
  </SidebarMenuItem>
);
