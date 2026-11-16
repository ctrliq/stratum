import type { Event } from '~/api/k8s/k8sSchemas';

import type { EventRow, TypeFilter } from '../types';
import { isOfType, isRecentWarning } from './predicates';
import { eventNamespaceOf } from './selectors';

export const inEventNamespace = (events: Event[], namespace: string | undefined): Event[] =>
  namespace === undefined
    ? events
    : events.filter((event) => eventNamespaceOf(event) === namespace);

export const ofType = <TRow extends Pick<EventRow, 'type'>>(
  rows: TRow[],
  filter: TypeFilter,
): TRow[] => rows.filter((row) => isOfType(row, filter));

export const recentWarningsIn = <TRow extends Pick<EventRow, 'type' | 'lastSeenAt'>>(
  rows: TRow[],
  now: number,
): TRow[] => rows.filter((row) => isRecentWarning(row, now));
