import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';

import { NodesList } from '~/domain/nodes/pages/NodesList';

const searchSchema = z.object({ node: z.string().optional() });

function NodesRoute() {
  const { node } = Route.useSearch();
  const navigate = Route.useNavigate();
  return (
    <NodesList
      selectedName={node}
      onSelected={(name) => {
        void navigate({ search: (previous) => ({ ...previous, node: name }) });
      }}
    />
  );
}

export const Route = createFileRoute('/c/$cluster/nodes')({
  validateSearch: searchSchema,
  component: NodesRoute,
});
