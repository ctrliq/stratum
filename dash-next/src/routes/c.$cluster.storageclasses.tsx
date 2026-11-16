import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { StorageClassesList } from '~/domain/storage/pages/StorageClassesList';

const searchSchema = z.object({ name: z.string().optional() });

function StorageClassesRoute() {
  const { name } = Route.useSearch();
  return <StorageClassesList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/storageclasses')({
  validateSearch: searchSchema,
  component: StorageClassesRoute,
});
