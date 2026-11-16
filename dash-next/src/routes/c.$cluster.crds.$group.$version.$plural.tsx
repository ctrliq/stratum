import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { CrdInstancesList } from '~/domain/crds/pages/CrdInstancesList';

const searchSchema = z.object({ name: z.string().optional() });

function CrdInstancesRoute() {
  const { group, version, plural } = Route.useParams();
  const { name } = Route.useSearch();
  return <CrdInstancesList group={group} version={version} plural={plural} linkedName={name} />;
}

export const Route = createFileRoute('/c/$cluster/crds/$group/$version/$plural')({
  validateSearch: searchSchema,
  component: CrdInstancesRoute,
});
