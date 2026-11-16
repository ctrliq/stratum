import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { PodDisruptionBudgetsList } from '~/domain/config/pages/PodDisruptionBudgetsList';

const searchSchema = z.object({ name: z.string().optional() });

function PodDisruptionBudgetsRoute() {
  const { name } = Route.useSearch();
  return <PodDisruptionBudgetsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/poddisruptionbudgets')({
  validateSearch: searchSchema,
  component: PodDisruptionBudgetsRoute,
});
