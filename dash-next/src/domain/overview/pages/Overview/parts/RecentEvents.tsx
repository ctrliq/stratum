import { LucideIconInfo, LucideIconTriangleAlert, Panel, PanelHeader } from '@ctrliq/quantic-react';
import type { MouseEvent } from 'react';

import type { EventRow } from '~/domain/events/types';
import { toRepeatText, toWarningCount } from '~/domain/events/utils/formatters';
import { isWarning } from '~/domain/events/utils/predicates';

import { overviewEventLimit } from '../../../constants';
import styles from './RecentEvents.module.scss';

type Props = {
  events: EventRow[];
  eventsHref: string;
  onLinkClicked: (event: MouseEvent, href: string) => void;
};

export function RecentEvents({ events, eventsHref, onLinkClicked }: Props) {
  return (
    <Panel>
      <PanelHeader slot="header">
        Recent Events
        <span slot="subtitle">{toWarningCount(events.filter(isWarning).length)}</span>
        <a
          slot="meta"
          href={eventsHref}
          onClick={(event) => {
            onLinkClicked(event, eventsHref);
          }}
        >
          All events
        </a>
      </PanelHeader>
      {events.length === 0 ? (
        <p className={styles.note}>No recent events.</p>
      ) : (
        <ul className={styles.events}>
          {events.slice(0, overviewEventLimit).map((event, index) => (
            <li
              key={`${event.namespace}/${event.object}/${event.reason}/${index}`}
              className={styles.event}
            >
              {isWarning(event) ? (
                <LucideIconTriangleAlert
                  className={styles.warning}
                  size="sm"
                  aria-label="Warning"
                />
              ) : (
                <LucideIconInfo size="sm" aria-label="Information" />
              )}
              <strong>{event.reason}</strong>
              {event.count > 1 ? <span>{toRepeatText(event.count)}</span> : null}
              <span>{event.age}</span>
              <span className={styles.object}>{event.object}</span>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}
