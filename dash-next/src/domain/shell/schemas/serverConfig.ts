import { z } from 'zod';

import type { ServerConfig } from '~/shared/context/ConfigContext';

const tokenProvider = 'token';
const defaultKubernetesBase = '/k8s';

const authSchema = z.object({
  required: z.boolean(),
  providers: z.array(z.string()).default([]),
});

const common = {
  version: z.string().default(''),
  k8sBase: z.string().default(defaultKubernetesBase),
};

const cisControlSchema = z.looseObject({
  id: z.string(),
  desc: z.string(),
  note: z.string().optional(),
  group: z.string(),
  source: z.string(),
  tag: z.enum(['exception', 'cis']).optional(),
});

const dashboardConfigSchema = z.object({
  ...common,
  mode: z.literal('standalone'),
  auth: authSchema.refine(({ providers }) => providers.includes(tokenProvider)),
  features: z
    .looseObject({
      nodeShell: z.boolean().default(false),
      nodeShellImage: z.string().default(''),
      secretSummaries: z.boolean().default(false),
    })
    .prefault({}),
  links: z.looseObject({ docs: z.string().default(''), pro: z.string().default('') }).prefault({}),
});

const fleetConfigSchema = z.object({
  ...common,
  mode: z.enum(['fleet', 'standalone']),
  auth: authSchema,
  features: z
    .looseObject({
      capi: z.boolean().default(false),
      kubeconfigSource: z.boolean().default(false),
    })
    .prefault({}),
  links: z.record(z.string(), z.string()).default({}),
  cisProfile: z.array(cisControlSchema).catch([]),
});

export const serverConfigSchema: z.ZodType<ServerConfig, unknown> = z.union([
  dashboardConfigSchema,
  fleetConfigSchema,
]);

export const toServerConfig = (answer: unknown): ServerConfig => serverConfigSchema.parse(answer);
