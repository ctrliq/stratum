import { z } from 'zod';

export const shellSchema = z.object({
  pod: z.string().min(1),
  command: z.string(),
});

export type ShellValues = z.infer<typeof shellSchema>;
