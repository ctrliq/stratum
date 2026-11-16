import { Button } from '@ctrliq/quantic-react';
import type { MouseEvent } from 'react';

type Props = {
  isBusy: boolean;
  onPressed: () => void;
};

export const QuickInstall = ({ isBusy, onPressed }: Props) => (
  <Button
    variant="outline"
    size="sm"
    disabled={isBusy}
    onClick={(event: MouseEvent) => {
      event.stopPropagation();
      onPressed();
    }}
  >
    Install
  </Button>
);
