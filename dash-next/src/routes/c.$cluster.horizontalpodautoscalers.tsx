import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { HorizontalPodAutoscalersList } from '~/domain/config/pages/HorizontalPodAutoscalersList';

const searchSchema = z.object({ name: z.string().optional() });

function HorizontalPodAutoscalersRoute() {
  const { name } = Route.useSearch();
  return <HorizontalPodAutoscalersList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/horizontalpodautoscalers')({
  validateSearch: searchSchema,
  component: HorizontalPodAutoscalersRoute,
});
