import { isRecord } from '~/shared/utils/predicates';

import type { SecretTable } from '../types/config';

export const isSecretTable = (value: unknown): value is SecretTable =>
  isRecord(value) && Array.isArray(value.columnDefinitions) && Array.isArray(value.rows);
