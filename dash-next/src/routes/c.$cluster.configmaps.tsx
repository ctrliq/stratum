import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { ConfigMapsList } from '~/domain/config/pages/ConfigMapsList';

const searchSchema = z.object({ name: z.string().optional() });

function ConfigMapsRoute() {
  const { name } = Route.useSearch();
  return <ConfigMapsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/configmaps')({
  validateSearch: searchSchema,
  component: ConfigMapsRoute,
});
