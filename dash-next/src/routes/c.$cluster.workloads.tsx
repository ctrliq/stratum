import { createFileRoute } from '@tanstack/react-router';

import { WorkloadsOverview } from '~/domain/workloads/pages/WorkloadsOverview';

export const Route = createFileRoute('/c/$cluster/workloads')({
  component: WorkloadsOverview,
});
