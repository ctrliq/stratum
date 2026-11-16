import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { RoleBindingsList } from '~/domain/access/pages/RoleBindingsList';

const searchSchema = z.object({ name: z.string().optional() });

function RoleBindingsRoute() {
  const { name } = Route.useSearch();
  return <RoleBindingsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/rolebindings')({
  validateSearch: searchSchema,
  component: RoleBindingsRoute,
});
