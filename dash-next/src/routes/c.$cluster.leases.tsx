import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { LeasesList } from '~/domain/config/pages/LeasesList';

const searchSchema = z.object({ name: z.string().optional() });

function LeasesRoute() {
  const { name } = Route.useSearch();
  return <LeasesList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/leases')({
  validateSearch: searchSchema,
  component: LeasesRoute,
});
