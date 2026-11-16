import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { PodsList } from '~/domain/workloads/pages/PodsList';

const searchSchema = z.object({ name: z.string().optional() });

function PodsListRoute() {
  const { name } = Route.useSearch();
  return <PodsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/pods')({
  validateSearch: searchSchema,
  component: PodsListRoute,
});
