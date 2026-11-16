import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { ClusterRoleBindingsList } from '~/domain/access/pages/ClusterRoleBindingsList';

const searchSchema = z.object({ name: z.string().optional() });

function ClusterRoleBindingsRoute() {
  const { name } = Route.useSearch();
  return <ClusterRoleBindingsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/clusterrolebindings')({
  validateSearch: searchSchema,
  component: ClusterRoleBindingsRoute,
});
