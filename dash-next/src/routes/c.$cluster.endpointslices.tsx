import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { EndpointSlicesList } from '~/domain/networking/pages/EndpointSlicesList';

const searchSchema = z.object({ name: z.string().optional() });

function EndpointSlicesRoute() {
  const { name } = Route.useSearch();
  return <EndpointSlicesList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/endpointslices')({
  validateSearch: searchSchema,
  component: EndpointSlicesRoute,
});
