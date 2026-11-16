import { DialogSize } from '@ctrliq/quantic-components';
import { Dialog } from '@ctrliq/quantic-react';

import type { Fleet, PaletteAction } from '../../types/screen';
import { PaletteEntries } from './PaletteEntries';

type Props = {
  fleet: Fleet;
  actions: PaletteAction[];
  isOpen: boolean;
  onClosed: () => void;
};

export const CommandPalette = ({ fleet, actions, isOpen, onClosed }: Props) => (
  <Dialog
    open={isOpen}
    size={DialogSize.Medium}
    aria-label="Command palette"
    onOpenChanged={(event) => {
      if (!event.detail.open) {
        onClosed();
      }
    }}
  >
    {isOpen ? <PaletteEntries fleet={fleet} actions={actions} onClosed={onClosed} /> : null}
  </Dialog>
);
