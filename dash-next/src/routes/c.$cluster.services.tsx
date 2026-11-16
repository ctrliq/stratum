import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { ServicesList } from '~/domain/networking/pages/ServicesList';

const searchSchema = z.object({ name: z.string().optional() });

function ServicesRoute() {
  const { name } = Route.useSearch();
  return <ServicesList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/services')({
  validateSearch: searchSchema,
  component: ServicesRoute,
});
