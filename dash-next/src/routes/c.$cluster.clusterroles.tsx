import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { ClusterRolesList } from '~/domain/access/pages/ClusterRolesList';

const searchSchema = z.object({ name: z.string().optional() });

function ClusterRolesRoute() {
  const { name } = Route.useSearch();
  return <ClusterRolesList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/clusterroles')({
  validateSearch: searchSchema,
  component: ClusterRolesRoute,
});
