import { createFileRoute, Outlet } from '@tanstack/react-router';

import { ClusterShell } from '~/domain/shell/components/ClusterShell/ClusterShell';
import { ScreenNotFound } from '~/domain/shell/components/NotFound/ScreenNotFound';
import { fromClusterSearch } from '~/domain/shell/utils/parsers';
import { TerminalHost } from '~/domain/terminal/components/TerminalHost/TerminalHost';

function ClusterRoute() {
  return (
    <ClusterShell clusterId={Route.useParams().cluster}>
      <TerminalHost>
        <Outlet />
      </TerminalHost>
    </ClusterShell>
  );
}

export const Route = createFileRoute('/c/$cluster')({
  validateSearch: fromClusterSearch,
  component: ClusterRoute,
  notFoundComponent: ScreenNotFound,
});
