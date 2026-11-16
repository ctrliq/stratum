import type { ReactNode } from 'react';

import { isDashboardServer } from '~/domain/auth/utils/predicates';
import { ScreenNotFound } from '~/domain/shell/components/NotFound/ScreenNotFound';
import { useServerConfig } from '~/shared/context/ConfigContext';

type Props = {
  children: ReactNode;
};

export const HubOnly = ({ children }: Props) =>
  isDashboardServer(useServerConfig()) ? <ScreenNotFound /> : children;
