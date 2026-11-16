import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { ReplicaSetsList } from '~/domain/workloads/pages/ReplicaSetsList';

const searchSchema = z.object({ name: z.string().optional() });

function ReplicaSetsListRoute() {
  const { name } = Route.useSearch();
  return <ReplicaSetsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/replicasets')({
  validateSearch: searchSchema,
  component: ReplicaSetsListRoute,
});
