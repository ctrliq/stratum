import { operationParam } from '../constants';

export const fromOperationsQuery = (searchStr: string): string[] => [
  ...new Set(new URLSearchParams(searchStr).getAll(operationParam).filter(Boolean)),
];
