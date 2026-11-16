import { z } from 'zod';

export const statsSummarySchema = z.object({
  pods: z.array(
    z.object({
      podRef: z.object({ namespace: z.string(), name: z.string() }),
      cpu: z.object({ usageNanoCores: z.number().optional() }).optional(),
      memory: z.object({ workingSetBytes: z.number().optional() }).optional(),
    }),
  ),
});
