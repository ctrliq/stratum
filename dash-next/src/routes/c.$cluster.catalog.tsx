import { createFileRoute } from '@tanstack/react-router';

import { Catalog } from '~/domain/catalog/pages/Catalog';

export const Route = createFileRoute('/c/$cluster/catalog')({
  component: Catalog,
});
