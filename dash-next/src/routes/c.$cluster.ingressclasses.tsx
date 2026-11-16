import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { IngressClassesList } from '~/domain/networking/pages/IngressClassesList';

const searchSchema = z.object({ name: z.string().optional() });

function IngressClassesRoute() {
  const { name } = Route.useSearch();
  return <IngressClassesList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/ingressclasses')({
  validateSearch: searchSchema,
  component: IngressClassesRoute,
});
