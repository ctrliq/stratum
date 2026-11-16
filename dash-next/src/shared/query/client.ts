import { QueryClient } from '@tanstack/react-query';

import { queryDefaults } from './polling';

export const queryClient = new QueryClient({ defaultOptions: queryDefaults });
