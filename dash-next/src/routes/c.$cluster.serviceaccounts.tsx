import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { ServiceAccountsList } from '~/domain/access/pages/ServiceAccountsList';

const searchSchema = z.object({ name: z.string().optional() });

function ServiceAccountsRoute() {
  const { name } = Route.useSearch();
  return <ServiceAccountsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/serviceaccounts')({
  validateSearch: searchSchema,
  component: ServiceAccountsRoute,
});
