import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { DaemonSetsList } from '~/domain/workloads/pages/DaemonSetsList';

const searchSchema = z.object({ name: z.string().optional() });

function DaemonSetsListRoute() {
  const { name } = Route.useSearch();
  return <DaemonSetsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/daemonsets')({
  validateSearch: searchSchema,
  component: DaemonSetsListRoute,
});
