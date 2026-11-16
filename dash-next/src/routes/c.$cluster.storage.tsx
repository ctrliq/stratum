import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { StorageTabs } from '~/domain/storage/pages/StorageTabs';

const searchSchema = z.object({ name: z.string().optional(), tab: z.string().optional() });

function StorageRoute() {
  const { name, tab } = Route.useSearch();
  const navigate = Route.useNavigate();
  return (
    <StorageTabs
      tab={tab}
      linkedName={name}
      onTabChosen={(chosen) =>
        void navigate({ search: (previous) => ({ ...previous, tab: chosen, name: undefined }) })
      }
    />
  );
}

export const Route = createFileRoute('/c/$cluster/storage')({
  validateSearch: searchSchema,
  component: StorageRoute,
});
