import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { MutatingWebhooksList } from '~/domain/config/pages/MutatingWebhooksList';

const searchSchema = z.object({ name: z.string().optional() });

function MutatingWebhooksRoute() {
  const { name } = Route.useSearch();
  return <MutatingWebhooksList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/mutatingwebhookconfigurations')({
  validateSearch: searchSchema,
  component: MutatingWebhooksRoute,
});
