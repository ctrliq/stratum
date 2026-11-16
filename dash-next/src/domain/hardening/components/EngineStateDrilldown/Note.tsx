import { Alert } from '@ctrliq/quantic-react';
import type { ReactNode } from 'react';

type Props = {
  children: string;
  action?: ReactNode;
};

export const Note = ({ children, action }: Props) => (
  <Alert type="warning">
    <span slot="title">{children}</span>
    {action}
  </Alert>
);
