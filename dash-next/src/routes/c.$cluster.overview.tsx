import { createFileRoute } from '@tanstack/react-router';

import { Overview } from '~/domain/overview/pages/Overview';

export const Route = createFileRoute('/c/$cluster/overview')({
  component: Overview,
});
