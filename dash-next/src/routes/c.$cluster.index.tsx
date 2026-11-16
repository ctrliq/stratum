import { createFileRoute } from '@tanstack/react-router';

import { ClusterHome } from '~/domain/shell/components/RootRedirect/ClusterHome';

export const Route = createFileRoute('/c/$cluster/')({
  component: ClusterHome,
});
