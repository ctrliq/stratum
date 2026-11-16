import type { ReactNode } from 'react';

import { FleetShell } from '~/domain/shell/components/ClusterShell/ClusterShell';
import { RootRedirect } from '~/domain/shell/components/RootRedirect/RootRedirect';
import { screens } from '~/domain/shell/constants';
import { isScreenOffered } from '~/domain/shell/utils/predicates';
import { useServerConfig } from '~/shared/context/ConfigContext';

type Props = {
  screenId: string;
  children: ReactNode;
};

export const FleetScreen = ({ screenId, children }: Props) => {
  const config = useServerConfig();
  const isOffered = screens.some(
    (screen) => screen.id === screenId && isScreenOffered(screen, config),
  );
  return isOffered ? <FleetShell>{children}</FleetShell> : <RootRedirect />;
};
