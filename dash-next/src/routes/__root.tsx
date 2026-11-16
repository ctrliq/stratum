import { createRootRoute, Outlet } from '@tanstack/react-router';

import { SessionProvider } from '~/domain/auth/components/SessionProvider/SessionProvider';
import { ConfigGate } from '~/domain/shell/components/ConfigGate/ConfigGate';
import { RootRedirect } from '~/domain/shell/components/RootRedirect/RootRedirect';
import { ConnectingSplash } from '~/domain/shell/components/Splash/ConnectingSplash';
import { Toaster } from '~/shared/components/Toaster/Toaster';

export const Route = createRootRoute({
  component: () => (
    <>
      <ConfigGate>
        <SessionProvider resolving={<ConnectingSplash />}>
          <Outlet />
        </SessionProvider>
      </ConfigGate>
      <Toaster />
    </>
  ),
  notFoundComponent: RootRedirect,
});
