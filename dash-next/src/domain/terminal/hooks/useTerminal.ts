import { createContext, useContext } from 'react';

import type { TerminalActions } from '../types/terminal';

const unmounted: TerminalActions = {
  isExecOffered: false,
  isNodeShellOffered: false,
  onLogsOpened: () => undefined,
  onExecOpened: () => undefined,
  onNodeShellOpened: () => undefined,
  onShellDialogOpened: () => undefined,
};

export const TerminalContext = createContext<TerminalActions>(unmounted);

export const useTerminal = (): TerminalActions => useContext(TerminalContext);
