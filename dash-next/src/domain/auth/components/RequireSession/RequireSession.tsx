import { Navigate } from '@tanstack/react-router';
import type { ReactNode } from 'react';

import { useSession } from '../../hooks/useSession';

type Props = {
  children: ReactNode;
};

export const RequireSession = ({ children }: Props) => {
  const { session } = useSession();
  return session.status === 'signedOut' ? <Navigate to="/login" replace /> : children;
};
