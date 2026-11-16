import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { NamespacesList } from '~/domain/namespaces/pages/NamespacesList';

const searchSchema = z.object({ name: z.string().optional() });

function NamespacesRoute() {
  const { name } = Route.useSearch();
  return <NamespacesList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/namespaces')({
  validateSearch: searchSchema,
  component: NamespacesRoute,
});
