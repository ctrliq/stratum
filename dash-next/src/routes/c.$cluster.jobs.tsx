import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { JobsList } from '~/domain/workloads/pages/JobsList';

const searchSchema = z.object({ name: z.string().optional() });

function JobsListRoute() {
  const { name } = Route.useSearch();
  return <JobsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/jobs')({
  validateSearch: searchSchema,
  component: JobsListRoute,
});
