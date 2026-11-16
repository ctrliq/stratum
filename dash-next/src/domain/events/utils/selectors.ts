import type { Event } from '~/api/k8s/k8sSchemas';
import { creationTimeOf, namespaceOf } from '~/shared/utils/selectors';

export const lastSeenOf = (
  event: Pick<Event, 'lastTimestamp' | 'eventTime' | 'metadata'>,
): string | undefined => event.lastTimestamp ?? event.eventTime ?? creationTimeOf(event);

export const involvedObjectOf = ({ involvedObject }: Pick<Event, 'involvedObject'>): string =>
  `${(involvedObject.kind ?? '').toLowerCase()}/${involvedObject.name ?? ''}`;

export const lastSeenTimeOf = (
  event: Pick<Event, 'lastTimestamp' | 'eventTime' | 'metadata'>,
): number => Date.parse(lastSeenOf(event) ?? '') || 0;

export const eventNamespaceOf = (
  event: Pick<Event, 'metadata' | 'involvedObject'>,
): string | undefined => namespaceOf(event) ?? event.involvedObject.namespace;
