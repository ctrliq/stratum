import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { ReplicationControllersList } from '~/domain/workloads/pages/ReplicationControllersList';

const searchSchema = z.object({ name: z.string().optional() });

function ReplicationControllersListRoute() {
  const { name } = Route.useSearch();
  return <ReplicationControllersList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/replicationcontrollers')({
  validateSearch: searchSchema,
  component: ReplicationControllersListRoute,
});
