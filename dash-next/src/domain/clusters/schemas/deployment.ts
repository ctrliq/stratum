import { z } from 'zod';

import { controlPlaneBounds, workerBounds } from '../constants';
import { isClusterName, isKubernetesVersion, isPlacementText } from '../utils/predicates';

export const deploymentSchema = z.object({
  provider: z.string(),
  name: z.string().refine(isClusterName),
  displayName: z.string(),
  version: z.string().refine(isKubernetesVersion),
  controlPlane: z.number().int().min(controlPlaneBounds.min).max(controlPlaneBounds.max),
  workers: z.number().int().min(workerBounds.min).max(workerBounds.max),
  placement: z.string().refine(isPlacementText),
});
