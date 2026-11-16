import { matchesFilter } from './predicates';

export const matchingFilter = (lines: string[], filter: string): string[] =>
  filter === '' ? lines : lines.filter((line) => matchesFilter(line, filter));
