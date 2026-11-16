import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { EndpointsList } from '~/domain/networking/pages/EndpointsList';

const searchSchema = z.object({ name: z.string().optional() });

function EndpointsRoute() {
  const { name } = Route.useSearch();
  return <EndpointsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/endpoints')({
  validateSearch: searchSchema,
  component: EndpointsRoute,
});
