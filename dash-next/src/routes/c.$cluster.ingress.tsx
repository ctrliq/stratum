import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { IngressesList } from '~/domain/networking/pages/IngressesList';

const searchSchema = z.object({ name: z.string().optional() });

function IngressesRoute() {
  const { name } = Route.useSearch();
  return <IngressesList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/ingress')({
  validateSearch: searchSchema,
  component: IngressesRoute,
});
