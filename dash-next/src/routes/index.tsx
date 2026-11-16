import { createFileRoute } from '@tanstack/react-router';

import { RootRedirect } from '~/domain/shell/components/RootRedirect/RootRedirect';

export const Route = createFileRoute('/')({
  component: RootRedirect,
});
