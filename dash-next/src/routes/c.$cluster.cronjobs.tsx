import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { CronJobsList } from '~/domain/workloads/pages/CronJobsList';

const searchSchema = z.object({ name: z.string().optional() });

function CronJobsListRoute() {
  const { name } = Route.useSearch();
  return <CronJobsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/cronjobs')({
  validateSearch: searchSchema,
  component: CronJobsListRoute,
});
