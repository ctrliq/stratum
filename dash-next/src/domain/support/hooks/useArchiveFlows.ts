import { createContext, useContext } from 'react';

import type { ArchiveFlows } from '../types/archive';

export const ArchiveFlowsContext = createContext<ArchiveFlows | undefined>(undefined);

export const useArchiveFlows = (): ArchiveFlows | undefined => useContext(ArchiveFlowsContext);
