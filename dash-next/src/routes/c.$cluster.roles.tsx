import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { RolesList } from '~/domain/access/pages/RolesList';

const searchSchema = z.object({ name: z.string().optional() });

function RolesRoute() {
  const { name } = Route.useSearch();
  return <RolesList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/roles')({
  validateSearch: searchSchema,
  component: RolesRoute,
});
