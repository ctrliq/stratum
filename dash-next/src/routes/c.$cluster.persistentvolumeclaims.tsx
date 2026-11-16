import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { PersistentVolumeClaimsList } from '~/domain/storage/pages/PersistentVolumeClaimsList';

const searchSchema = z.object({ name: z.string().optional() });

function PersistentVolumeClaimsRoute() {
  const { name } = Route.useSearch();
  return <PersistentVolumeClaimsList linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/persistentvolumeclaims')({
  validateSearch: searchSchema,
  component: PersistentVolumeClaimsRoute,
});
