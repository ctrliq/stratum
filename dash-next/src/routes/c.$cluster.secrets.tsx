import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { SecretsList } from '~/domain/config/pages/SecretsList';

const searchSchema = z.object({ name: z.string().optional() });

function SecretsRoute() {
  const { name } = Route.useSearch();
  return <SecretsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/secrets')({
  validateSearch: searchSchema,
  component: SecretsRoute,
});
