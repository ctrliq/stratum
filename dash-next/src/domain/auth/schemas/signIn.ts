import { z } from 'zod';

export const tokenSignInSchema = z.object({
  token: z.string().trim().min(1, 'Paste a service account token.'),
});

export const passwordSignInSchema = z.object({
  username: z.string().trim().min(1, 'Enter your username.'),
  password: z.string().min(1, 'Enter your password.'),
});
