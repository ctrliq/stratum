import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { RuntimeClassesList } from '~/domain/config/pages/RuntimeClassesList';

const searchSchema = z.object({ name: z.string().optional() });

function RuntimeClassesRoute() {
  const { name } = Route.useSearch();
  return <RuntimeClassesList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/runtimeclasses')({
  validateSearch: searchSchema,
  component: RuntimeClassesRoute,
});
