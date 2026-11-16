import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { ResourceQuotasList } from '~/domain/config/pages/ResourceQuotasList';

const searchSchema = z.object({ name: z.string().optional() });

function ResourceQuotasRoute() {
  const { name } = Route.useSearch();
  return <ResourceQuotasList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/resourcequotas')({
  validateSearch: searchSchema,
  component: ResourceQuotasRoute,
});
