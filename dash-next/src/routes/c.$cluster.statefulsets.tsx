import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { StatefulSetsList } from '~/domain/workloads/pages/StatefulSetsList';

const searchSchema = z.object({ name: z.string().optional() });

function StatefulSetsListRoute() {
  const { name } = Route.useSearch();
  return <StatefulSetsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/statefulsets')({
  validateSearch: searchSchema,
  component: StatefulSetsListRoute,
});
