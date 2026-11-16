import { createFileRoute } from '@tanstack/react-router';

import { HelmReleasesList } from '~/domain/helm/pages/HelmReleasesList';

export const Route = createFileRoute('/c/$cluster/helm')({
  component: HelmReleasesList,
});
