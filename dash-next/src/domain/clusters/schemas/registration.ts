import { z } from 'zod';

import { agentProfiles, registrationMethods, tokenLifetimes } from '../constants';
import { isClusterName } from '../utils/predicates';

export const registrationSchema = z
  .object({
    method: z.enum(registrationMethods),
    name: z.string().refine(isClusterName),
    displayName: z.string(),
    profile: z.enum(agentProfiles),
    lifetime: z.enum(tokenLifetimes),
    kubeconfig: z.string(),
    context: z.string(),
  })
  .refine(({ method, kubeconfig }) => method === 'agent' || kubeconfig.trim() !== '', {
    path: ['kubeconfig'],
  });
