import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { CrdsList } from '~/domain/crds/pages/CrdsList';

const searchSchema = z.object({ name: z.string().optional() });

function CrdsRoute() {
  const { name } = Route.useSearch();
  return <CrdsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/crds/')({
  validateSearch: searchSchema,
  component: CrdsRoute,
});
