import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { AccessTabs } from '~/domain/access/pages/AccessTabs';

const searchSchema = z.object({ name: z.string().optional(), tab: z.string().optional() });

function RbacRoute() {
  const { name, tab } = Route.useSearch();
  const navigate = Route.useNavigate();
  return (
    <AccessTabs
      tab={tab}
      linkedName={name}
      onTabChosen={(chosen) =>
        void navigate({ search: (previous) => ({ ...previous, tab: chosen, name: undefined }) })
      }
    />
  );
}

export const Route = createFileRoute('/c/$cluster/rbac')({
  validateSearch: searchSchema,
  component: RbacRoute,
});
