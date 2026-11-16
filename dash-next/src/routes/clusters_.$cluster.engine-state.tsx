import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { HubOnly } from '~/domain/hardening/components/HubOnly/HubOnly';
import { EngineStateDetail } from '~/domain/hardening/pages/EngineStateDetail';
import { FleetShell } from '~/domain/shell/components/ClusterShell/ClusterShell';

const searchSchema = z.object({ q: z.string().optional().catch(undefined) });

function EngineStateRoute() {
  return (
    <FleetShell>
      <HubOnly>
        <EngineStateDetail clusterId={Route.useParams().cluster} />
      </HubOnly>
    </FleetShell>
  );
}

export const Route = createFileRoute('/clusters_/$cluster/engine-state')({
  validateSearch: searchSchema,
  component: EngineStateRoute,
});
