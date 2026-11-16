import type { Event } from '~/api/k8s/k8sSchemas';
import { toAge, toDisplayText } from '~/shared/utils/formatters';

import { defaultEventType, eventRowLimit, warningType } from '../constants';
import type { EventGroup, EventRow } from '../types';
import { isWarning } from './predicates';
import { eventNamespaceOf, involvedObjectOf, lastSeenOf, lastSeenTimeOf } from './selectors';

export const toEventRow = (event: Event, now: number): EventRow => ({
  type: event.type ?? defaultEventType,
  reason: event.reason ?? '',
  object: involvedObjectOf(event),
  namespace: toDisplayText(eventNamespaceOf(event)),
  message: event.message ?? '',
  count: event.count ?? 1,
  age: toAge(lastSeenOf(event), now),
  lastSeenAt: lastSeenTimeOf(event),
});

export const toEventRows = (events: Event[], now: number): EventRow[] =>
  [...events]
    .sort((first, second) => lastSeenTimeOf(second) - lastSeenTimeOf(first))
    .slice(0, eventRowLimit)
    .map((event) => toEventRow(event, now));

const toEventGroup = (rows: EventRow[]): EventGroup => {
  const latest = rows.reduce((newest, row) => (row.lastSeenAt > newest.lastSeenAt ? row : newest));
  return {
    type: rows.some(isWarning) ? warningType : latest.type,
    reason: latest.reason,
    objectCount: new Set(rows.map(({ namespace, object }) => `${namespace}/${object}`)).size,
    count: rows.reduce((total, { count }) => total + count, 0),
    age: latest.age,
    lastSeenAt: latest.lastSeenAt,
  };
};

export const toEventGroups = (rows: EventRow[]): EventGroup[] =>
  [...new Set(rows.map(({ reason }) => reason))]
    .map((reason) => toEventGroup(rows.filter((row) => row.reason === reason)))
    .sort((first, second) => second.lastSeenAt - first.lastSeenAt);
