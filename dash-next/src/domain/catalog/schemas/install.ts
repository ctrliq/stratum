import { z } from 'zod';

import { namespaceRule, releaseNameRule, requiredRule, secretNameRule } from '../constants';
import type { CatalogApp, InstallValues } from '../types/catalog';
import { isHelmReleaseName, isNamespaceName, isSecretNameAccepted } from '../utils/predicates';
import { unmetParamsOf } from '../utils/selectors';

export const toInstallSchema = (app: Pick<CatalogApp, 'params'>) =>
  z
    .object({
      releaseName: z.string().refine(isHelmReleaseName, releaseNameRule),
      targetNamespace: z.string().refine(isNamespaceName, namespaceRule),
      createNamespace: z.boolean(),
      values: z.string(),
      credentialsSecret: z.string().refine(isSecretNameAccepted, secretNameRule),
      params: z.record(z.string(), z.union([z.string(), z.number(), z.boolean(), z.null()])),
    })
    .refine((values) => unmetParamsOf(app, values).length === 0, {
      path: ['params'],
      message: requiredRule,
    });

export const isInstallable = (app: Pick<CatalogApp, 'params'>, values: InstallValues): boolean =>
  toInstallSchema(app).safeParse(values).success;

export const brokenRulesOf = (
  app: Pick<CatalogApp, 'params'>,
  values: InstallValues,
): Record<string, string> =>
  Object.fromEntries(
    (toInstallSchema(app).safeParse(values).error?.issues ?? []).map(({ path, message }) => [
      path.join('.'),
      message,
    ]),
  );
