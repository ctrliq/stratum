import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { HelmChartsList } from '~/domain/helm/pages/HelmChartsList';

const searchSchema = z.object({ name: z.string().optional() });

function HelmChartsRoute() {
  const { name } = Route.useSearch();
  return <HelmChartsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/helmcharts')({
  validateSearch: searchSchema,
  component: HelmChartsRoute,
});
