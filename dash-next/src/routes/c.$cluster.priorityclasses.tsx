import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { PriorityClassesList } from '~/domain/config/pages/PriorityClassesList';

const searchSchema = z.object({ name: z.string().optional() });

function PriorityClassesRoute() {
  const { name } = Route.useSearch();
  return <PriorityClassesList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/priorityclasses')({
  validateSearch: searchSchema,
  component: PriorityClassesRoute,
});
