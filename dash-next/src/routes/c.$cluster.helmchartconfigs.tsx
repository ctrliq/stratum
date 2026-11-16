import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { HelmChartConfigsList } from '~/domain/helm/pages/HelmChartConfigsList';

const searchSchema = z.object({ name: z.string().optional() });

function HelmChartConfigsRoute() {
  const { name } = Route.useSearch();
  return <HelmChartConfigsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/helmchartconfigs')({
  validateSearch: searchSchema,
  component: HelmChartConfigsRoute,
});
