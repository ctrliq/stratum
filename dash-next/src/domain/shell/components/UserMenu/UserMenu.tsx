import {
  BadgeColor,
  ButtonSize,
  ButtonVariant,
  DropdownMenuSize,
  IconSize,
  PopoverPlacement,
} from '@ctrliq/quantic-components';
import {
  Badge,
  Button,
  DropdownMenu,
  DropdownMenuItem,
  LucideIconCircleUser,
  LucideIconLogOut,
} from '@ctrliq/quantic-react';

import { useGetCodeVersion } from '~/api/k8s/k8sComponents';
import { useSession } from '~/domain/auth/hooks/useSession';
import { useSignOut } from '~/domain/auth/hooks/useSignOut';
import { isReadOnly } from '~/domain/auth/utils/predicates';
import { identityOf } from '~/domain/auth/utils/selectors';
import { useServerConfig } from '~/shared/context/ConfigContext';

import { useScreenPlace } from '../../hooks/useScreenPlace';
import type { Fleet } from '../../types/screen';
import { toButtonName, toIdentityLine, toUserName, toVersionLine } from '../../utils/formatters';
import { clusterIn } from '../../utils/selectors';
import styles from './UserMenu.module.scss';

type Props = {
  fleet: Fleet;
};

export const UserMenu = ({ fleet }: Props) => {
  const config = useServerConfig();
  const identity = identityOf(useSession().session);
  const signOut = useSignOut();
  const { clusterId } = useScreenPlace();
  const served = useGetCodeVersion(
    {},
    { enabled: clusterId !== undefined, staleTime: Infinity, select: (info) => info.gitVersion },
  );
  const userName = toUserName(config, identity);

  return (
    <DropdownMenu
      placement={PopoverPlacement.BottomEnd}
      size={DropdownMenuSize.Small}
      onItemSelected={() => {
        signOut.mutate();
      }}
    >
      <Button slot="trigger" variant={ButtonVariant.Ghost} size={ButtonSize.Small} title={userName}>
        <LucideIconCircleUser size={IconSize.Small} />
        {toButtonName(userName)}
      </Button>
      <p className={styles.identity}>
        {toIdentityLine(config, identity)}
        {isReadOnly(config, identity) ? <Badge color={BadgeColor.Gray}>Read-only</Badge> : null}
      </p>
      {config.auth.required ? (
        <DropdownMenuItem value="sign-out" label="Sign out" disabled={signOut.isPending}>
          <LucideIconLogOut size={IconSize.Small} />
          Sign out
        </DropdownMenuItem>
      ) : null}
      <p className={styles.version}>
        {toVersionLine(config, clusterIn(fleet, clusterId)?.k8sVersion || served.data)}
      </p>
    </DropdownMenu>
  );
};
