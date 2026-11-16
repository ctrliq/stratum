import type { LoginSearch } from '../types/session';
import { isReturnTarget, isSsoFailureCode } from './predicates';

export const fromLoginSearch = (search: Record<string, unknown>): LoginSearch => ({
  ...(isSsoFailureCode(search.error) ? { error: search.error } : {}),
  ...(isReturnTarget(search.return) ? { return: search.return } : {}),
});
