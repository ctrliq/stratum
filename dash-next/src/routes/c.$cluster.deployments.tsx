import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { DeploymentsList } from '~/domain/workloads/pages/DeploymentsList';

const searchSchema = z.object({ name: z.string().optional() });

function DeploymentsListRoute() {
  const { name } = Route.useSearch();
  return <DeploymentsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/deployments')({
  validateSearch: searchSchema,
  component: DeploymentsListRoute,
});
