import { z } from 'zod';

import { fromReplicaText } from '../utils/parsers';
import { isReplicaCount } from '../utils/predicates';

export const scaleSchema = z.object({
  replicas: z
    .string()
    .refine(
      (text) => isReplicaCount(fromReplicaText(text)),
      'Enter a whole number of replicas, zero or more.',
    ),
});
