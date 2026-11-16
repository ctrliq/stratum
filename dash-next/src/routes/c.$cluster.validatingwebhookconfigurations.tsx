import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { ValidatingWebhooksList } from '~/domain/config/pages/ValidatingWebhooksList';

const searchSchema = z.object({ name: z.string().optional() });

function ValidatingWebhooksRoute() {
  const { name } = Route.useSearch();
  return <ValidatingWebhooksList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/validatingwebhookconfigurations')({
  validateSearch: searchSchema,
  component: ValidatingWebhooksRoute,
});
