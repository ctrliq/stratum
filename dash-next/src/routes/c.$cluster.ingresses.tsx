import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { IngressesList } from '~/domain/networking/pages/IngressesList';

const searchSchema = z.object({ name: z.string().optional() });

function IngressesAliasRoute() {
  const { name } = Route.useSearch();
  return <IngressesList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/ingresses')({
  validateSearch: searchSchema,
  component: IngressesAliasRoute,
});
