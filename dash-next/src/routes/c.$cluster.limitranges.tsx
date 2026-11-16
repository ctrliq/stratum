import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { LimitRangesList } from '~/domain/config/pages/LimitRangesList';

const searchSchema = z.object({ name: z.string().optional() });

function LimitRangesRoute() {
  const { name } = Route.useSearch();
  return <LimitRangesList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/limitranges')({
  validateSearch: searchSchema,
  component: LimitRangesRoute,
});
