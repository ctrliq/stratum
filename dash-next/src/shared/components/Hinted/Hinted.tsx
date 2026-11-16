import { Tooltip } from '@ctrliq/quantic-react';
import type { ReactNode } from 'react';

type Props = {
  hint: string | undefined;
  children: ReactNode;
};

export function Hinted({ hint, children }: Props) {
  return hint ? (
    <Tooltip>
      {children}
      <span slot="content">{hint}</span>
    </Tooltip>
  ) : (
    children
  );
}
