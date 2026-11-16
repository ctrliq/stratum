import { createFileRoute } from '@tanstack/react-router';

import { EventsList } from '~/domain/events/pages/EventsList';

export const Route = createFileRoute('/c/$cluster/events')({
  component: EventsList,
});
