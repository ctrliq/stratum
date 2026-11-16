import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { FleetScreen } from '~/domain/clusters/components/FleetScreen/FleetScreen';
import { ClustersList } from '~/domain/clusters/pages/ClustersList';

const searchSchema = z.object({ q: z.string().optional().catch(undefined) });

function ClustersRoute() {
  const { q } = Route.useSearch();
  const navigate = Route.useNavigate();
  return (
    <FleetScreen screenId="clusters">
      <ClustersList
        search={q ?? ''}
        onSearchChanged={(search) =>
          void navigate({ search: { q: search || undefined }, replace: true })
        }
      />
    </FleetScreen>
  );
}

export const Route = createFileRoute('/clusters')({
  validateSearch: searchSchema,
  component: ClustersRoute,
});
