import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { PersistentVolumesList } from '~/domain/storage/pages/PersistentVolumesList';

const searchSchema = z.object({ name: z.string().optional() });

function PersistentVolumesRoute() {
  const { name } = Route.useSearch();
  return <PersistentVolumesList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/persistentvolumes')({
  validateSearch: searchSchema,
  component: PersistentVolumesRoute,
});
