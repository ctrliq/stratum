import { createFileRoute } from '@tanstack/react-router';

import { AuthLogin } from '~/domain/auth/pages/AuthLogin';
import { fromLoginSearch } from '~/domain/auth/utils/parsers';

function LoginRoute() {
  return <AuthLogin search={Route.useSearch()} />;
}

export const Route = createFileRoute('/login')({
  validateSearch: fromLoginSearch,
  component: LoginRoute,
});
